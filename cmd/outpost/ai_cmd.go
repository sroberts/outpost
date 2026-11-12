package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/mail"
	"github.com/sroberts/outpost/internal/secrets"
	"github.com/sroberts/outpost/internal/supervisor"
)

// withAI is the supervisor's Secrets with OUTPOST_AI_URL and the app's own
// OUTPOST_AI_TOKEN added, and OUTPOST_MAIL_URL and OUTPOST_MAIL_TOKEN, so
// every app is started with them and restarted when a token is regenerated,
// by the same revision check that restarts it for a rotated secret.
// Outpost's own variables are set last, so a secret can't replace them;
// secrets can't be named OUTPOST_ anyway.
type withAI struct {
	secrets supervisor.Secrets
	ai      *ai.Service
	mail    *mail.Service // nil leaves mail out
}

func (w withAI) Resolve(service string) (map[string]string, string) {
	vars, rev := w.secrets.Resolve(service)
	aiVars, aiRev := w.ai.Resolve(service)
	out := make(map[string]string, len(vars)+len(aiVars)+2)
	for k, v := range vars {
		out[k] = v
	}
	for k, v := range aiVars {
		out[k] = v
	}
	rev += "|" + aiRev
	if w.mail != nil {
		mailVars, mailRev := w.mail.Resolve(service)
		for k, v := range mailVars {
			out[k] = v
		}
		rev += "|" + mailRev
	}
	return out, rev
}

const aiUsage = `usage:
  outpost ai                          the models apps are given, and what each provider has
  outpost ai set llm|system1 <model> [--provider P]
                                      choose one, over config.toml's; P is ollama, openai,
                                      anthropic, openrouter or typesafe (default: the one in use)
  outpost ai reset llm|system1        go back to config.toml's
  outpost ai apps llm|system1 --all | --none | --reset | <app>...
                                      choose which apps may use it
  outpost ai key <provider> [--value-stdin]
                                      save a hosted provider's API key, read hidden
  outpost ai test llm|system1         send it one small request
  outpost ai regenerate <service>     give an app a new token; it restarts`

// cmdAI shows and chooses the models apps are given. Every subcommand goes
// through the daemon, which serves the endpoint and owns ai.db.
func (a *app) cmdAI(args []string) error {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return errors.New(aiUsage)
		}
		return a.cmdAIStatus()
	}
	sub, rest := args[0], args[1:]
	kindArg := func() (string, error) {
		if len(rest) == 0 || !ai.ValidKind(rest[0]) {
			return "", fmt.Errorf("ai %s: name the kind, llm or system1\n%s", sub, aiUsage)
		}
		return rest[0], nil
	}
	switch sub {
	case "set":
		kind, err := kindArg()
		if err != nil {
			return err
		}
		model, provider, err := setArgs(kind, rest[1:])
		if err != nil {
			return err
		}
		return a.daemonSay(func() (string, error) {
			c, err := a.daemon()
			if err != nil {
				return "", err
			}
			return c.AISetFrom(kind, provider, model)
		})
	case "apps":
		kind, err := kindArg()
		if err != nil {
			return err
		}
		every, reset, apps, err := appsArgs(kind, rest[1:])
		if err != nil {
			return err
		}
		return a.daemonSay(func() (string, error) {
			c, err := a.daemon()
			if err != nil {
				return "", err
			}
			if reset {
				return c.AIAppsReset(kind)
			}
			return c.AIApps(kind, every, apps)
		})
	case "key":
		if len(rest) == 0 || rest[0] == ai.ProviderOllama || !ai.ValidProvider(rest[0]) {
			return errors.New("usage: outpost ai key openai|anthropic|openrouter|typesafe [--value-stdin]")
		}
		provider, fromStdin := rest[0], false
		for _, f := range rest[1:] {
			if f != "--value-stdin" && f != "-value-stdin" {
				return fmt.Errorf("ai key: unexpected argument %q; the key is read hidden, or from stdin with --value-stdin", f)
			}
			fromStdin = true
		}
		c, err := a.daemon()
		if err != nil {
			return err
		}
		value, err := a.secretValue(ai.ProviderTitle(provider)+"'s API key", fromStdin)
		if err != nil {
			return err
		}
		return a.daemonSay(func() (string, error) { return c.AIKey(provider, value) })
	case "reset":
		kind, err := kindArg()
		if err != nil {
			return err
		}
		return a.daemonSay(func() (string, error) {
			c, err := a.daemon()
			if err != nil {
				return "", err
			}
			return c.AIReset(kind)
		})
	case "test":
		kind, err := kindArg()
		if err != nil {
			return err
		}
		c, err := a.daemon()
		if err != nil {
			return err
		}
		fmt.Fprintf(a.err, "Asking the %s; a model Ollama hasn't loaded takes a while the first time…\n", ai.KindName(kind))
		res, err := c.AITest(kind)
		if err != nil {
			return err
		}
		reply := res.Reply
		if reply == "" {
			reply = "(an empty reply)"
		}
		from := ""
		if res.Provider != "" && res.Provider != ai.ProviderOllama {
			from = ai.ProviderTitle(res.Provider) + "'s "
		}
		fmt.Fprintf(a.out, "%s%s answered in %s: %s\n", from, res.Model, res.Took.Round(100*time.Millisecond), reply)
		return nil
	case "regenerate":
		if len(rest) != 1 {
			return errors.New("usage: outpost ai regenerate <service>")
		}
		return a.daemonSay(func() (string, error) {
			c, err := a.daemon()
			if err != nil {
				return "", err
			}
			return c.AIRegenerate(rest[0])
		})
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, aiUsage)
		return nil
	}
	return fmt.Errorf("unknown ai command %q\n%s", sub, aiUsage)
}

// setArgs reads `<model> [--provider P]`, the flag on either side.
func setArgs(kind string, args []string) (model, provider string, err error) {
	use := fmt.Errorf("usage: outpost ai set %s <model> [--provider P], the model as `outpost ai` lists it", kind)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--provider" || a == "-provider":
			if i+1 >= len(args) {
				return "", "", use
			}
			i++
			provider = args[i]
		case strings.HasPrefix(a, "--provider=") || strings.HasPrefix(a, "-provider="):
			_, provider, _ = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-") || model != "":
			return "", "", use
		default:
			model = a
		}
	}
	if model == "" {
		return "", "", use
	}
	if provider != "" && !ai.ProviderServes(provider, kind) {
		return "", "", fmt.Errorf("ai set: %q can't serve the %s; the %s can come from %s", provider, ai.KindName(kind),
			ai.KindName(kind), strings.Join(servers(kind), ", "))
	}
	return model, provider, nil
}

// servers are the providers that serve kind.
func servers(kind string) []string {
	var out []string
	for _, p := range ai.ProviderNames {
		if ai.ProviderServes(p, kind) {
			out = append(out, p)
		}
	}
	return out
}

// appsArgs reads --all, --none, --reset, or app names.
func appsArgs(kind string, args []string) (every, reset bool, apps []string, err error) {
	use := fmt.Errorf("usage: outpost ai apps %s --all | --none | --reset | <app> [<app> ...]", kind)
	if len(args) == 0 {
		return false, false, nil, use
	}
	switch args[0] {
	case "--all", "-all":
		every = true
	case "--none", "-none":
	case "--reset", "-reset":
		reset = true
	default:
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				return false, false, nil, use
			}
		}
		return false, false, args, nil
	}
	if len(args) > 1 {
		return false, false, nil, use
	}
	return every, reset, nil, nil
}

func (a *app) daemonSay(f func() (string, error)) error {
	msg, err := f()
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func (a *app) cmdAIStatus() error {
	c, err := a.daemon()
	if err != nil {
		return err
	}
	o, err := c.AIStatus()
	if err != nil {
		return err
	}
	writeAIStatus(a.out, o)
	return nil
}

func writeAIStatus(w io.Writer, o ai.Overview) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range o.Choices {
		model := c.Model
		if model == "" {
			model = "none chosen"
		}
		var notes []string
		if len(o.Providers) > 0 && c.Model != "" {
			notes = append(notes, ai.ProviderTitle(orOllama(c.Provider)))
		}
		switch {
		case c.FromPortal && c.File != "":
			notes = append(notes, fmt.Sprintf("chosen by %s, over config.toml's %s", c.UpdatedBy, c.File))
		case c.FromPortal:
			notes = append(notes, "chosen by "+c.UpdatedBy)
		case c.Model != "":
			notes = append(notes, "from config.toml")
		}
		if c.Missing && orOllama(c.Provider) == ai.ProviderOllama {
			notes = append(notes, "NOT IN OLLAMA")
		} else if c.Missing {
			notes = append(notes, "NOT LISTED BY "+strings.ToUpper(ai.ProviderTitle(c.Provider)))
		}
		if c.NoKey {
			notes = append(notes, "NO API KEY")
		}
		if len(o.Providers) > 0 {
			switch {
			case c.Every:
			case len(c.Apps) == 0:
				notes = append(notes, "no app may use it")
			default:
				notes = append(notes, "only "+strings.Join(c.Apps, ", "))
			}
		}
		if len(c.APIs) == 1 && c.Kind == ai.LLM {
			notes = append(notes, "apps must call "+c.APIs[0])
		}
		note := ""
		if len(notes) > 0 {
			note = "(" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintf(tw, "%s:\t%s\t%s\n", ai.KindName(c.Kind), model, note)
	}
	_ = tw.Flush() // writing to the terminal
	fmt.Fprintf(w, "\nApps call %s with their OUTPOST_AI_TOKEN. Ollama is at %s.\n", o.URL, o.OllamaURL)

	if o.ModelsError != "" {
		fmt.Fprintf(w, "\nCan't list Ollama's models: %s\n", o.ModelsError)
	} else {
		fmt.Fprintln(w, "\nOllama has:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MODEL\tFOR\tPARAMETERS\tCAPABILITIES")
		for _, m := range o.Models {
			var kinds []string
			for _, k := range ai.Kinds {
				if m.Serves(k) {
					kinds = append(kinds, k)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, orDash(strings.Join(kinds, ", ")),
				orDash(m.ParameterSize), strings.Join(m.Capabilities, ", "))
		}
		_ = tw.Flush() // writing to the terminal
	}

	if len(o.Providers) > 0 {
		fmt.Fprintln(w, "\nProviders:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PROVIDER\tSERVES\tKEY\tMODELS")
		for _, p := range o.Providers {
			key := "not needed"
			if p.Keyed {
				key = "none: `outpost ai key " + p.Name + "`"
				if p.HasKey {
					key = "set (secret " + p.KeySecret + ")"
				}
			}
			var counts []string
			for _, cat := range o.Catalogs {
				if cat.Provider != p.Name {
					continue
				}
				switch {
				case cat.Error != "" && p.Name != ai.ProviderOllama:
					counts = append(counts, cat.Kind+": "+cat.Error)
				case cat.Error == "":
					counts = append(counts, fmt.Sprintf("%s: %d", cat.Kind, len(cat.Models)))
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, strings.Join(p.Kinds, ", "), key, orDash(strings.Join(counts, "; ")))
		}
		_ = tw.Flush() // writing to the terminal
	}

	if len(o.Usage) > 0 {
		fmt.Fprintln(w, "\nSince the daemon started:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "APP\tKIND\tREQUESTS\tFAILED\tTOKENS IN\tTOKENS OUT\tLAST ERROR")
		for _, u := range o.Usage {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n", u.Service, u.Kind, u.Requests, u.Errors,
				u.InputTokens, u.OutputTokens, orDash(u.LastError))
		}
		_ = tw.Flush() // writing to the terminal
	}
}

func orOllama(provider string) string {
	if provider == "" {
		return ai.ProviderOllama
	}
	return provider
}

// warnAIModels logs, once, what would make apps' AI requests fail: a
// provider not answering, a chosen model it doesn't have, or a key missing.
func warnAIModels(ctx context.Context, s *ai.Service, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o := s.Overview(ctx)
	for _, c := range o.Choices {
		if c.Model == "" {
			continue
		}
		title := ai.ProviderTitle(c.Provider)
		switch {
		case c.Provider == ai.ProviderOllama && o.ModelsError != "":
			fmt.Fprintf(out, "outpost: AI models: the %s is Ollama's %s, but %s\n", ai.KindName(c.Kind), c.Model, o.ModelsError)
		case c.NoKey:
			fmt.Fprintf(out, "outpost: AI models: the %s is %s's %s, and there's no API key for %s; add one with `outpost ai key %s`\n",
				ai.KindName(c.Kind), title, c.Model, title, c.Provider)
		case c.Missing && c.Provider == ai.ProviderOllama:
			fmt.Fprintf(out, "outpost: AI models: the %s is %s, which Ollama doesn't have; pull it with `ollama pull %s`, or choose another\n",
				ai.KindName(c.Kind), c.Model, c.Model)
		case c.Missing:
			fmt.Fprintf(out, "outpost: AI models: the %s is %s's %s, which %s doesn't list; choose another\n",
				ai.KindName(c.Kind), title, c.Model, title)
		}
	}
}

// aiOptions are config.toml's [ai] as the AI service takes them.
func aiOptions(c config.AI, path string, keys ai.Keys, logf func(string, ...any)) ai.Options {
	apps := func(p *[]string) []string {
		if p == nil {
			return nil // every app
		}
		return append([]string{}, *p...) // perhaps none, which isn't every
	}
	providers := map[string]ai.ProviderConfig{}
	for name, p := range c.Providers() {
		providers[name] = ai.ProviderConfig{BaseURL: p.BaseURL, KeySecret: p.APIKeySecret}
	}
	return ai.Options{
		Path:            path,
		OllamaURL:       c.OllamaBase(),
		Listen:          c.ListenAddr(),
		LLM:             c.LLM,
		LLMProvider:     c.LLMProvider,
		LLMApps:         apps(c.LLMApps),
		System1:         c.System1,
		System1Provider: c.System1Provider,
		System1Apps:     apps(c.System1Apps),
		Providers:       providers,
		Keys:            keys,
		Logf:            logf,
	}
}

// secretKeys keeps the hosted providers' API keys in secrets.db, as secrets
// bound to no service: shown on the Secrets page, rotatable there, and never
// in an app's environment unless the operator binds one on purpose.
type secretKeys struct{ store *secrets.Store }

func (k secretKeys) Value(name string) (string, bool) { return k.store.Value(name) }

func (k secretKeys) Put(name, value string) error {
	if err := secrets.ValidValue(value); err != nil {
		return err
	}
	if _, ok := k.store.Get(name); ok {
		_, err := k.store.Rotate(name, value)
		return err
	}
	_, err := k.store.Create(name, secrets.DefaultEnv(name), value)
	return err
}
