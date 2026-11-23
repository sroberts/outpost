package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
)

const mailUsage = `usage:
  outpost mail                         the account, and each app's addresses
  outpost mail account <address>       set the Gmail account, app password read hidden (--password-stdin)
  outpost mail check                   read the inbox now
  outpost mail alias add <service>     make the app another address (--note N)
  outpost mail alias rm <service> <+route>
  outpost mail send <service> on|off   whether the app may send as the account
  outpost mail token <service>         give the app a new mail token; it restarts`

// cmdMail manages the mail apps get through the Gmail account, as the
// portal's Mail page and each app's Mail panel do. Everything goes through
// the daemon, which owns mail.db.
func (a *app) cmdMail(args []string) error {
	if len(args) == 0 {
		return a.cmdMailStatus()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		if len(rest) > 0 {
			return fmt.Errorf("mail status: unexpected argument %q", rest[0])
		}
		return a.cmdMailStatus()
	case "account":
		return a.cmdMailAccount(rest)
	case "check":
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.MailCheck() })
	case "alias":
		return a.cmdMailAlias(rest)
	case "send":
		if len(rest) != 2 || (rest[1] != "on" && rest[1] != "off") {
			return errors.New("usage: outpost mail send <service> on|off")
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.MailSend(rest[0], rest[1] == "on") })
	case "token", "regenerate":
		if len(rest) != 1 {
			return errors.New("usage: outpost mail token <service>")
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.MailRegenerate(rest[0]) })
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, mailUsage)
		return nil
	default:
		return fmt.Errorf("unknown mail command %q\n%s", sub, mailUsage)
	}
}

func (a *app) cmdMailAlias(args []string) error {
	if len(args) == 0 {
		return errors.New(mailUsage)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("mail alias add", flag.ContinueOnError)
		fs.SetOutput(a.err)
		note := fs.String("note", "", "what the address is for, as a reminder")
		service, err := parseServiceCommand(fs, "mail alias add", args[1:])
		if err != nil {
			return err
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.MailAliasAdd(service, *note) })
	case "rm", "remove":
		if len(args) != 3 {
			return errors.New("usage: outpost mail alias rm <service> <+route>")
		}
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.MailAliasRemove(args[1], args[2]) })
	}
	return fmt.Errorf("unknown mail alias command %q\n%s", args[0], mailUsage)
}

func (a *app) cmdMailAccount(args []string) error {
	fs := flag.NewFlagSet("mail account", flag.ContinueOnError)
	fs.SetOutput(a.err)
	fromStdin := fs.Bool("password-stdin", false, "read the app password from stdin, less one trailing newline")
	address, err := parseServiceCommand(fs, "mail account", args)
	if err != nil {
		if strings.HasSuffix(err.Error(), "a service name is required") {
			return errors.New("mail account: the Gmail address is required")
		}
		return err
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	var password string
	switch {
	case *fromStdin:
		b, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return fmt.Errorf("--password-stdin: %w", err)
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	case stdinIsTerminal():
		// Never from the command line, where it would land in shell
		// history and in ps.
		fmt.Fprintf(a.err, "App password for %s: ", address)
		password, err = readSecret()
		fmt.Fprintln(a.err)
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
	default:
		return errors.New("stdin is not a terminal: pass --password-stdin to read the app password from it")
	}
	msg, err := c.MailAccount(address, password)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func (a *app) cmdMailStatus() error {
	c, err := a.daemon()
	if err != nil {
		return err
	}
	o, err := c.MailStatus()
	if err != nil {
		return err
	}
	return writeMailStatus(a.out, o, time.Now())
}

// writeMailStatus prints the account, then a row per app.
func writeMailStatus(w io.Writer, o ipc.MailOverview, now time.Time) error {
	st := o.Account
	switch {
	case !st.Ready():
		fmt.Fprintln(w, "account   not set up; set it with `outpost mail account <address>`")
	default:
		fmt.Fprintf(w, "account   %s\n", st.Address)
		switch {
		case st.Error != "":
			fmt.Fprintf(w, "inbox     can't be read: %s\n", st.Error)
		case st.Checked.IsZero():
			fmt.Fprintln(w, "inbox     checking")
		default:
			fmt.Fprintf(w, "inbox     checked %s ago; %d routed since the daemon started\n",
				now.Sub(st.Checked).Round(time.Second), st.Routed)
		}
	}
	if o.Secret != "" {
		fmt.Fprintf(w, "password  kept as the secret %s\n", o.Secret)
	}
	if len(o.Apps) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tADDRESSES\tSENDS\tWAITING")
	for _, app := range o.Apps {
		var addrs []string
		for _, r := range app.Routes {
			a := r.Address
			if a == "" {
				a = "+" + r.Route
			}
			if r.Note != "" {
				a += " (" + r.Note + ")"
			}
			addrs = append(addrs, a)
		}
		sends := "no"
		if app.Send {
			sends = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", app.Name, orDash(strings.Join(addrs, ", ")), sends, app.Held)
	}
	return tw.Flush()
}
