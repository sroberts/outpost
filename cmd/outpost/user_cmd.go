package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
)

// Password input is reached through these so tests can stand in for a
// terminal without one.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readSecret                = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
)

const userUsage = `usage:
  outpost user add <name> [--admin] [--email E] [--display-name N] [--group G]... [--no-password | --password-stdin]
  outpost user passwd <name> [--password-stdin]
  outpost user rm <name>
  outpost user list
  outpost user groups <name> +group -group ...
  outpost user disable <name>
  outpost user enable <name>
  outpost user sessions <name>
  outpost user logout <name>
  outpost user admin <name> on|off`

// cmdUser manages the people who can log in. Every subcommand goes through
// the daemon: it owns auth.db, and ending a session has to reach it.
func (a *app) cmdUser(args []string) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return a.cmdUserAdd(rest)
	case "passwd":
		return a.cmdUserPasswd(rest)
	case "rm", "remove":
		return a.userSimple(rest, "rm", (*ipc.Client).UserRemove)
	case "list", "ls":
		return a.cmdUserList(rest)
	case "groups":
		return a.cmdUserGroups(rest)
	case "disable":
		return a.userSimple(rest, "disable", (*ipc.Client).UserDisable)
	case "enable":
		return a.userSimple(rest, "enable", (*ipc.Client).UserEnable)
	case "sessions":
		return a.cmdUserSessions(rest)
	case "logout":
		return a.userSimple(rest, "logout", (*ipc.Client).UserLogout)
	case "admin":
		return a.cmdUserAdmin(rest)
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, userUsage)
		return nil
	default:
		return fmt.Errorf("unknown user command %q\n%s", sub, userUsage)
	}
}

// cmdAuth holds auth-wide operations; today, only key rotation.
func (a *app) cmdAuth(args []string) error {
	if len(args) != 1 || args[0] != "rotate-key" {
		return errors.New("usage: outpost auth rotate-key")
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := c.AuthRotateKey()
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// daemon returns a client, or the standard explanation when nothing is
// listening. The CLI never opens auth.db itself.
func (a *app) daemon() (*ipc.Client, error) {
	c := a.client()
	if !c.Available() {
		return nil, errNoDaemon()
	}
	return c, nil
}

// repeated collects a flag given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func (a *app) cmdUserAdd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	fs.SetOutput(a.err)
	admin := fs.Bool("admin", false, "make the user an admin")
	email := fs.String("email", "", "email address, passed to apps")
	display := fs.String("display-name", "", "display name, passed to apps")
	var groups repeated
	fs.Var(&groups, "group", "add the user to a group (repeatable)")
	noPassword := fs.Bool("no-password", false, "create the user without a password; set one later with `outpost user passwd`")
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	name, err := parseNamed(fs, "user add", args)
	if err != nil {
		return err
	}
	if *noPassword && *fromStdin {
		return errors.New("user add: --no-password and --password-stdin can't be used together")
	}

	c, err := a.daemon()
	if err != nil {
		return err
	}
	password := ""
	if !*noPassword {
		if password, err = a.newPassword(name, *fromStdin); err != nil {
			return err
		}
	}
	msg, err := c.UserAdd(ipc.UserRequest{
		Username: name, DisplayName: *display, Email: *email,
		Admin: *admin, Password: password, AddGroups: groups,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func (a *app) cmdUserPasswd(args []string) error {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	fs.SetOutput(a.err)
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	name, err := parseNamed(fs, "user passwd", args)
	if err != nil {
		return err
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	password, err := a.newPassword(name, *fromStdin)
	if err != nil {
		return err
	}
	msg, err := c.UserPasswd(name, password)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// newPassword gets a password from the terminal, asking twice, or from one
// line of stdin. There's deliberately no flag for it: a password on the
// command line ends up in shell history and the process table. The length
// rule is checked here so a typo costs a retype, not a round trip.
func (a *app) newPassword(username string, fromStdin bool) (string, error) {
	var pw string
	switch {
	case fromStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", errors.New("--password-stdin: no password on stdin")
		}
		pw = strings.TrimRight(line, "\r\n")

	case stdinIsTerminal():
		fmt.Fprintf(a.err, "New password for %s: ", username)
		first, err := readSecret()
		fmt.Fprintln(a.err)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if err := auth.ValidPassword(first); err != nil {
			return "", err
		}
		fmt.Fprint(a.err, "Repeat it: ")
		second, err := readSecret()
		fmt.Fprintln(a.err)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if first != second {
			return "", errors.New("the passwords don't match; nothing was changed")
		}
		pw = first

	default:
		return "", errors.New("stdin is not a terminal: pass --password-stdin to read the password from it" +
			" (or, for user add, --no-password to set one later)")
	}
	if err := auth.ValidPassword(pw); err != nil {
		return "", err
	}
	return pw, nil
}

func (a *app) cmdUserList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("user list: unexpected argument %q", args[0])
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	users, err := c.UserList()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Fprintln(a.out, "No users yet. Create the first admin with `outpost user add <name> --admin`.")
		return nil
	}
	return writeUserTable(a.out, users)
}

func writeUserTable(w io.Writer, users []ipc.UserInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tADMIN\tGROUPS\tSTATUS\tLAST LOGIN\tSESSIONS")
	for _, u := range users {
		admin := ""
		if u.Admin {
			admin = "yes"
		}
		status := "active"
		switch {
		case u.Disabled:
			status = "disabled"
		case !u.HasPassword:
			status = "no password"
		}
		last := "never"
		if !u.LastLoginAt.IsZero() {
			last = u.LastLoginAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
			u.Username, orDash(admin), orDash(strings.Join(u.Groups, ",")), status, last, u.Sessions)
	}
	return tw.Flush()
}

// cmdUserSessions lists where a user is signed in, as their account page
// does.
func (a *app) cmdUserSessions(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: outpost user sessions <name>")
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	list, err := c.UserSignIns(args[0])
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(a.out, "%s isn't signed in anywhere.\n", args[0])
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tFROM\tSIGNED IN\tLAST SEEN\tAPPS")
	for _, si := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", gateway.DescribeAgent(si.UserAgent), orDash(si.IP),
			si.CreatedAt.Local().Format("2006-01-02 15:04"), si.LastSeen.Local().Format("2006-01-02"),
			orDash(strings.Join(si.Apps, ",")))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\nLast seen is to the day. `outpost user logout %s` ends them all.\n", args[0])
	return nil
}

// cmdUserGroups takes +group and -group arguments. It parses them by hand,
// because the flag package would read "-kids" as an unknown flag.
func (a *app) cmdUserGroups(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: outpost user groups <name> [+group] [-group]")
	}
	name := args[0]
	var add, remove []string
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "+") && len(arg) > 1:
			add = append(add, arg[1:])
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			remove = append(remove, arg[1:])
		default:
			return fmt.Errorf("user groups: %q needs a + (add) or - (remove) in front", arg)
		}
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := c.UserGroups(name, add, remove)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// cmdUserAdmin grants or revokes admin. Without it the last-admin guard's
// advice, make another user an admin, had no command to follow it.
func (a *app) cmdUserAdmin(args []string) error {
	const use = "usage: outpost user admin <name> on|off"
	if len(args) != 2 {
		return errors.New(use)
	}
	var admin bool
	switch args[1] {
	case "on":
		admin = true
	case "off":
	default:
		return fmt.Errorf("user admin: %q is not on or off\n%s", args[1], use)
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := c.UserAdmin(args[0], admin)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// userSimple runs a subcommand whose only argument is the username.
func (a *app) userSimple(args []string, verb string, call func(*ipc.Client, string) (string, error)) error {
	fs := flag.NewFlagSet("user "+verb, flag.ContinueOnError)
	fs.SetOutput(a.err)
	name, err := parseNamed(fs, "user "+verb, args)
	if err != nil {
		return err
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := call(c, name)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// parseNamed is parseServiceCommand for commands that take a username, so the
// error says what's missing.
func parseNamed(fs *flag.FlagSet, verb string, args []string) (string, error) {
	name, err := parseServiceCommand(fs, verb, args)
	if err != nil && strings.HasSuffix(err.Error(), "a service name is required") {
		return "", fmt.Errorf("%s: a username is required", verb)
	}
	return name, err
}
