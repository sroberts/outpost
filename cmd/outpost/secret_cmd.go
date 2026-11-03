package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/secrets"
)

const secretUsage = `usage:
  outpost secret list
  outpost secret set <name> [--env VAR] [--service S]... [--global] [--value-stdin]
  outpost secret rm <name>
  outpost secret bind <name> <service>|--global
  outpost secret unbind <name> <service>|--global`

// cmdSecret manages the secrets services get as environment variables. Every
// subcommand goes through the daemon, which owns secrets.db and restarts the
// services a change reaches. A value is never taken from the command line,
// where it would land in shell history and in ps.
func (a *app) cmdSecret(args []string) error {
	if len(args) == 0 {
		return errors.New(secretUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		if len(rest) > 0 {
			return fmt.Errorf("secret list: unexpected argument %q", rest[0])
		}
		return a.cmdSecretList()
	case "set":
		return a.cmdSecretSet(rest)
	case "rm", "remove":
		if len(rest) != 1 {
			return errors.New("usage: outpost secret rm <name>")
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.SecretRemove(rest[0]) })
	case "bind", "unbind":
		name, service, err := bindArgs(sub, rest)
		if err != nil {
			return err
		}
		if sub == "bind" {
			return a.secretDo(func(c *ipc.Client) (string, error) { return c.SecretBind(name, service) })
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.SecretUnbind(name, service) })
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, secretUsage)
		return nil
	default:
		return fmt.Errorf("unknown secret command %q\n%s", sub, secretUsage)
	}
}

// bindArgs reads <name> <service> or <name> --global; "" is every service.
func bindArgs(verb string, args []string) (string, string, error) {
	use := fmt.Errorf("usage: outpost secret %s <name> <service>|--global", verb)
	if len(args) != 2 {
		return "", "", use
	}
	switch {
	case args[0] == "--global" || args[0] == "-global":
		return args[1], "", nil
	case args[1] == "--global" || args[1] == "-global":
		return args[0], "", nil
	case strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-"):
		return "", "", use
	}
	return args[0], args[1], nil
}

func (a *app) secretDo(f func(*ipc.Client) (string, error)) error {
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := f(c)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func (a *app) cmdSecretSet(args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(a.err)
	env := fs.String("env", "", "the environment variable it's handed over as; default is the name in upper case")
	var services repeated
	fs.Var(&services, "service", "bind it to this service (repeatable)")
	global := fs.Bool("global", false, "bind it to every service")
	fromStdin := fs.Bool("value-stdin", false, "read the value from stdin, all of it, less one trailing newline")
	name, err := parseServiceCommand(fs, "secret set", args)
	if err != nil {
		if strings.HasSuffix(err.Error(), "a service name is required") {
			return errors.New("secret set: a secret name is required")
		}
		return err
	}
	if err := secrets.ValidName(name); err != nil {
		return err
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	value, err := a.secretValue(name, *fromStdin)
	if err != nil {
		return err
	}
	msg, err := c.SecretSet(name, *env, value)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	if *global {
		services = append(services, "")
	}
	for _, svc := range services {
		msg, err := c.SecretBind(name, svc)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.out, msg)
	}
	return nil
}

// secretValue reads the value from a hidden prompt, or from stdin when asked.
// Unlike a password it isn't asked for twice: it's pasted, not typed, and
// rotating it again is how a mistake is put right.
func (a *app) secretValue(name string, fromStdin bool) (string, error) {
	switch {
	case fromStdin:
		b, err := io.ReadAll(io.LimitReader(stdin, secrets.MaxValue+2))
		if err != nil {
			return "", fmt.Errorf("--value-stdin: %w", err)
		}
		v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		return v, secrets.ValidValue(v)
	case stdinIsTerminal():
		fmt.Fprintf(a.err, "Value for %s: ", name)
		v, err := readSecret()
		fmt.Fprintln(a.err)
		if err != nil {
			return "", fmt.Errorf("read value: %w", err)
		}
		return v, secrets.ValidValue(v)
	}
	return "", errors.New("stdin is not a terminal: pass --value-stdin to read the value from it")
}

func (a *app) cmdSecretList() error {
	c, err := a.daemon()
	if err != nil {
		return err
	}
	list, err := c.SecretList()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(a.out, "No secrets. Add one with `outpost secret set <name> --service <service>`, or from the portal's Secrets page.")
		return nil
	}
	return writeSecretTable(a.out, list)
}

func writeSecretTable(w io.Writer, list []ipc.SecretInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVARIABLE\tBOUND TO\tREACHES\tUPDATED")
	for _, s := range list {
		bound := append([]string(nil), s.Services...)
		if s.Global {
			bound = append([]string{"every service"}, bound...)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Env,
			orDash(strings.Join(bound, ", ")), orDash(strings.Join(s.Reaches, ", ")),
			s.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}
