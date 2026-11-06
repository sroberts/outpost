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
	"github.com/sroberts/outpost/internal/supervisor"
)

// withAI is the supervisor's Secrets with OUTPOST_AI_URL and the app's own
// OUTPOST_AI_TOKEN added, so every app is started with them and restarted
// when its token is regenerated, by the same revision check that restarts it
// for a rotated secret. Outpost's own variables are set last, so a secret
// can't replace them; secrets can't be named OUTPOST_ anyway.
type withAI struct {
	secrets supervisor.Secrets
	ai      *ai.Service
}

func (w withAI) Resolve(service string) (map[string]string, string) {
	vars, rev := w.secrets.Resolve(service)
	aiVars, aiRev := w.ai.Resolve(service)
	out := make(map[string]string, len(vars)+len(aiVars))
	for k, v := range vars {
		out[k] = v
	}
	for k, v := range aiVars {
		out[k] = v
	}
	return out, rev + "|" + aiRev
}

const aiUsage = `usage:
  outpost ai                          the models apps are given, and what Ollama has
  outpost ai set llm|system1 <model>  choose one, over config.toml's
  outpost ai reset llm|system1        go back to config.toml's
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
		if len(rest) != 2 {
			return fmt.Errorf("usage: outpost ai set %s <model>, as `outpost ai` lists it", kind)
		}
		return a.daemonSay(func() (string, error) {
			c, err := a.daemon()
			if err != nil {
				return "", err
			}
			return c.AISet(kind, rest[1])
		})
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
		fmt.Fprintf(a.out, "%s answered in %s: %s\n", res.Model, res.Took.Round(100*time.Millisecond), reply)
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
		switch {
		case c.FromPortal && c.File != "":
			notes = append(notes, fmt.Sprintf("chosen by %s, over config.toml's %s", c.UpdatedBy, c.File))
		case c.FromPortal:
			notes = append(notes, "chosen by "+c.UpdatedBy)
		case c.Model != "":
			notes = append(notes, "from config.toml")
		}
		if c.Missing {
			notes = append(notes, "NOT IN OLLAMA")
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

// warnAIModels logs, once, what would make apps' AI requests fail: Ollama
// not answering, or a chosen model it doesn't have.
func warnAIModels(ctx context.Context, s *ai.Service, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o := s.Overview(ctx)
	if o.ModelsError != "" {
		fmt.Fprintf(out, "outpost: AI models: %s\n", o.ModelsError)
		return
	}
	for _, c := range o.Choices {
		if c.Missing {
			fmt.Fprintf(out, "outpost: AI models: the %s is %s, which Ollama doesn't have; pull it with `ollama pull %s`, or choose another\n",
				ai.KindName(c.Kind), c.Model, c.Model)
		}
	}
}
