package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
)

const tokenUsage = `usage:
  outpost token create <user> [--name N] [--app A]... [--expires 90d]
  outpost token create <user> --internet --app A --expires 90d [--name N]
  outpost token list [<user>]
  outpost token revoke <id>
  outpost token revoke-client <id>`

// cmdToken manages personal access tokens. Like cmdUser, every subcommand goes
// through the daemon, which owns auth.db.
func (a *app) cmdToken(args []string) error {
	if len(args) == 0 {
		return errors.New(tokenUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return a.cmdTokenCreate(rest)
	case "list", "ls":
		return a.cmdTokenList(rest)
	case "revoke":
		return a.cmdTokenRevoke(rest)
	case "revoke-client":
		return a.cmdTokenRevokeClient(rest)
	case "help", "-h", "--help":
		fmt.Fprintln(a.out, tokenUsage)
		return nil
	default:
		return fmt.Errorf("unknown token command %q\n%s", sub, tokenUsage)
	}
}

func (a *app) cmdTokenCreate(args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(a.err)
	name := fs.String("name", "", "what the token is for, shown by `outpost token list`")
	var apps repeated
	fs.Var(&apps, "app", "limit the token to an app (repeatable); default is every app the user may reach")
	expires := fs.String("expires", "", "how long until it expires, such as 90d, 12w or 24h; default is never")
	internet := fs.Bool("internet", false, "also accept it from the internet, for an app that puts its MCP endpoint there; needs one --app and --expires")
	user, err := parseNamed(fs, "token create", args)
	if err != nil {
		return err
	}
	// Refused before the daemon is asked, so the reason names the flags. The
	// daemon and the store check the same things.
	if *internet {
		if len(apps) != 1 {
			return errors.New("token create: --internet needs exactly one --app, since a token the internet can use should reach as little as possible")
		}
		if *expires == "" {
			return errors.New("token create: --internet needs --expires, such as --expires 90d")
		}
	}
	var ttl time.Duration
	if *expires != "" {
		if ttl, err = parseExpiry(*expires); err != nil {
			return fmt.Errorf("token create: %w", err)
		}
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	secret, t, warning, err := c.TokenCreate(ipc.TokenRequest{
		Username: user, Name: *name, Apps: apps, ExpiresIn: ttl, Internet: *internet,
	})
	if err != nil {
		return err
	}
	// Only the secret goes to stdout, so TOKEN=$(outpost token create ...) works
	// and the words around it are still seen.
	fmt.Fprintf(a.err, "Created access token %d for %s, reaching %s, expiring %s.\n",
		t.ID, t.Username, tokenReach(t), tokenExpiry(t))
	if t.Internet {
		fmt.Fprintln(a.err, "It also works from the internet, once the app puts its MCP endpoint there.")
	}
	if warning != "" {
		fmt.Fprintf(a.err, "Warning: %s.\n", warning)
	}
	fmt.Fprintln(a.err, "This is the only time it will be shown:")
	fmt.Fprintln(a.out, secret)
	return nil
}

func (a *app) cmdTokenList(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("token list: unexpected argument %q", args[1])
	}
	user := ""
	if len(args) == 1 {
		user = args[0]
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	tokens, err := c.TokenList(user)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Fprintln(a.out, "No access tokens. Make one with `outpost token create <user>`.")
	} else if err := writeTokenTable(a.out, tokens); err != nil {
		return err
	}
	clients, err := c.TokenClients(user)
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return nil
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Connected clients (OAuth; `outpost token revoke-client <id>` disconnects one):")
	return writeClientTable(a.out, clients)
}

func writeClientTable(w io.Writer, clients []ipc.ClientInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSER\tCLIENT\tAPP\tCONNECTED\tLAST RENEWED\tLAPSES")
	for _, g := range clients {
		renewed := "never"
		if !g.RefreshedAt.IsZero() {
			renewed = g.RefreshedAt.Local().Format("2006-01-02 15:04")
		}
		lapses := g.ExpiresAt.Local().Format("2006-01-02 15:04")
		if g.Lapsed {
			lapses = "lapsed"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			g.ID, g.Username, gateway.ClientLabel(g.ClientName, g.ClientID), g.App,
			g.CreatedAt.Local().Format("2006-01-02 15:04"), renewed, lapses)
	}
	return tw.Flush()
}

func writeTokenTable(w io.Writer, tokens []ipc.TokenInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSER\tNAME\tAPPS\tFROM\tCREATED\tEXPIRES\tLAST USED")
	for _, t := range tokens {
		used := "never"
		if !t.LastUsedAt.IsZero() {
			used = t.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		apps := "all"
		if len(t.Apps) > 0 {
			apps = strings.Join(t.Apps, ",")
		}
		from := "tailnet"
		if t.Internet {
			from = "internet"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Username, orDash(t.Name), apps, from,
			t.CreatedAt.Local().Format("2006-01-02 15:04"), tokenExpiry(t), used)
	}
	return tw.Flush()
}

func (a *app) cmdTokenRevoke(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: outpost token revoke <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("token revoke: %q is not a token id; `outpost token list` shows them", args[0])
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := c.TokenRevoke(id)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func (a *app) cmdTokenRevokeClient(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: outpost token revoke-client <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("token revoke-client: %q is not a client id; `outpost token list` shows them", args[0])
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	msg, err := c.TokenRevokeClient(id)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

func tokenReach(t ipc.TokenInfo) string {
	if len(t.Apps) == 0 {
		return "every app they may open"
	}
	return strings.Join(t.Apps, ", ")
}

func tokenExpiry(t ipc.TokenInfo) string {
	switch {
	case t.Expired:
		return "expired"
	case t.ExpiresAt.IsZero():
		return "never"
	}
	return t.ExpiresAt.Local().Format("2006-01-02 15:04")
}

// maxExpiryDays keeps days and weeks well inside what a time.Duration can hold,
// so a large number is refused rather than wrapping into a past expiry.
const maxExpiryDays = 36500

// parseExpiry reads an expiry: a whole number of days or weeks, which is how
// anyone thinks about a token's life, or anything time.ParseDuration takes.
func parseExpiry(s string) (time.Duration, error) {
	bad := fmt.Errorf("--expires %q: use a length of time in the future, such as 90d, 12w or 24h", s)
	perUnit := 0
	switch {
	case strings.HasSuffix(s, "d"):
		perUnit = 1
	case strings.HasSuffix(s, "w"):
		perUnit = 7
	default:
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, bad
		}
		return d, nil
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, bad
	}
	if n > maxExpiryDays/perUnit {
		return 0, fmt.Errorf("--expires %q is over %d days; leave it out for a token that doesn't expire", s, maxExpiryDays)
	}
	return time.Duration(n*perUnit) * 24 * time.Hour, nil
}
