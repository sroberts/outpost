package tui

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/supervisor"
)

// The rest of the socket API, in the pieces the palette reaches it by.
// *ipc.Client satisfies all of them; a client without one goes without the
// actions it backs.
type (
	ServiceOps interface {
		Forget(name string) (string, error)
		Apply() ([]supervisor.ServiceStatus, error)
		Backup(name string) ([]backup.Archive, string, error)
		Backups(name string) ([]backup.Archive, error)
		Restore(name, timestamp string) (string, error)
	}
	UserOps interface {
		UserAdd(r ipc.UserRequest) (string, error)
		UserPasswd(username, password string) (string, error)
		UserRemove(username string) (string, error)
		UserGroups(username string, add, remove []string) (string, error)
		UserDisable(username string) (string, error)
		UserEnable(username string) (string, error)
		UserAdmin(username string, admin bool) (string, error)
		UserTailnet(username, login string) (string, error)
		AuthRotateKey() (string, error)
	}
	TokenOps interface {
		TokenCreate(r ipc.TokenRequest) (string, ipc.TokenInfo, string, error)
		TokenList(username string) ([]ipc.TokenInfo, error)
		TokenRevoke(id int64) (string, error)
		TokenClients(username string) ([]ipc.ClientInfo, error)
		TokenRevokeClient(id int64) (string, error)
	}
	SecretOps interface {
		SecretList() ([]ipc.SecretInfo, error)
		SecretSet(name, env, value string) (string, error)
		SecretRemove(name string) (string, error)
		SecretBind(name, service string) (string, error)
		SecretUnbind(name, service string) (string, error)
	}
	AIRegenerator interface {
		AIRegenerate(service string) (string, error)
	}
	DaemonInfo interface {
		DaemonVersion() (string, error)
	}
)

func (m Model) serviceOps() (ServiceOps, bool) { x, ok := m.client.(ServiceOps); return x, ok }
func (m Model) userOps() (UserOps, bool)       { x, ok := m.client.(UserOps); return x, ok }
func (m Model) tokenOps() (TokenOps, bool)     { x, ok := m.client.(TokenOps); return x, ok }
func (m Model) secretOps() (SecretOps, bool)   { x, ok := m.client.(SecretOps); return x, ok }

func hasServiceOps(m Model) bool { _, ok := m.serviceOps(); return ok }
func hasUserOps(m Model) bool    { _, ok := m.userOps(); return ok }
func hasTokenOps(m Model) bool   { _, ok := m.tokenOps(); return ok }
func hasSecretOps(m Model) bool  { _, ok := m.secretOps(); return ok }
func hasRunner(m Model) bool     { return m.runner != nil }
func hasLogs(m Model) bool       { return m.logs != nil }
func hasRegen(m Model) bool      { _, ok := m.client.(AIRegenerator); return ok }

// do runs f and reports its message as a notice.
func do(verb, target string, f func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		msg, err := f()
		return actionMsg{verb: verb, service: target, message: msg, err: err}
	}
}

// show runs f and puts what it returns in the output pane.
func show(title string, f func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		text, err := f()
		return outputMsg{title: title, text: text, err: err}
	}
}

// cli runs an outpost subcommand through the Runner, its output to the pane.
func (m Model) cli(title string, args ...string) tea.Cmd {
	r := m.runner
	return show(title, func() (string, error) { return r(args...) })
}

// busy says something slow is under way, and runs it.
func busy(m Model, what string, cmd tea.Cmd) (Model, tea.Cmd) {
	m.notice, m.noticeIsErr = what+"…", false
	return m, cmd
}

func selectedService(m Model) vals { return vals{"service": m.selectedName()} }
func selectedUserVals(m Model) vals {
	return vals{"user": m.selectedUser(), "service": m.selectedName()}
}

// list splits a comma or space separated list.
func list(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

// parseExpiry reads 90d, 12w or anything time.ParseDuration does, as
// `outpost token create --expires` does.
func parseExpiry(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	bad := fmt.Errorf("expires %q: use a length of time such as 90d, 12w or 24h", s)
	if n, unit := s[:len(s)-1], s[len(s)-1]; unit == 'd' || unit == 'w' {
		v, err := strconv.Atoi(n)
		if err != nil || v <= 0 {
			return 0, bad
		}
		day := 24 * time.Hour
		if unit == 'w' {
			day *= 7
		}
		return time.Duration(v) * day, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, bad
	}
	return d, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// plainText drops terminal escapes from text the dashboard didn't write
// (a service's log, a command's output), which would otherwise restyle the
// screen or throw off every width measured over it.
func plainText(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 && r != '\n' {
			return -1
		}
		return r
	}, s)
}

// actions is everything the palette offers, grouped as the CLI's help is.
// Each says what it covers, and TestParity_* hold that set to the CLI's
// commands and the socket's API, so a new command can't quietly be missed.
func actions() []action {
	svcField := text("service", "service", "name")
	userField := text("user", "user", "username")
	return []action{
		// --- Services ---
		{id: "start", group: "Services", title: "start", about: "start a stopped service",
			fields: []formField{svcField}, prefill: selectedService,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.act("start", v["service"])
			}, covers: []string{"ipc:Start", "cli:start"}},
		{id: "stop", group: "Services", title: "stop", about: "stop a running service",
			fields: []formField{svcField}, prefill: selectedService,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.act("stop", v["service"])
			}, covers: []string{"ipc:Stop", "cli:stop"}},
		{id: "restart", group: "Services", title: "restart", about: "restart a service",
			fields: []formField{svcField}, prefill: selectedService,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.act("restart", v["service"])
			}, covers: []string{"ipc:Restart", "cli:restart"}},
		{id: "status", group: "Services", title: "status", about: "health, restarts and recent events, as `outpost status` prints them",
			fields: []formField{svcField}, prefill: selectedService, needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.cli("status "+v["service"], "status", v["service"])
			}, covers: []string{"cli:status", "cli:list", "ipc:List", "ipc:Status"}},
		{id: "apply", group: "Services", title: "apply", about: "validate the manifests and reconcile now",
			needs: func(m Model) bool { return hasRunner(m) || hasServiceOps(m) },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				if m.runner != nil {
					return busy(m, "applying", m.cli("apply", "apply"))
				}
				x, _ := m.serviceOps()
				return busy(m, "applying", do("apply", "", func() (string, error) {
					list, err := x.Apply()
					return fmt.Sprintf("applied: %d service(s)", len(list)), err
				}))
			}, covers: []string{"ipc:Apply", "cli:apply"}},
		{id: "logs", group: "Services", title: "logs", about: "follow a service's log, full screen",
			fields: []formField{svcField}, prefill: selectedService, needs: hasLogs,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				m.lv.service, m.lv.all, m.lv.events = v["service"], false, false
				m.lv.reset()
				return m.enterView(viewLogs)
			}, covers: []string{"cli:logs"}},
		{id: "events", group: "Services", title: "activity", about: "a service's activity, or every service's",
			fields: []formField{optional("service", "service", "every service")}, prefill: selectedService, needs: hasLogs,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				m.lv.service, m.lv.all, m.lv.events = v["service"], v["service"] == "", true
				m.lv.reset()
				return m.enterView(viewLogs)
			}, covers: []string{"ipc:Events"}},
		{id: "backup", group: "Services", title: "backup", about: "archive a service's storage; leave it blank for every service",
			fields: []formField{optional("service", "service", "every service")}, prefill: selectedService, needs: hasServiceOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.serviceOps()
				name := v["service"]
				return busy(m, "backing up "+orEvery(name), do("backup", name, func() (string, error) {
					_, msg, err := x.Backup(name)
					return msg, err
				}))
			}, covers: []string{"ipc:Backup", "cli:backup"}},
		{id: "backups", group: "Services", title: "backups", about: "list a service's backups",
			fields: []formField{svcField}, prefill: selectedService, needs: hasServiceOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.serviceOps()
				name := v["service"]
				return m, show("backups of "+name, func() (string, error) {
					list, err := x.Backups(name)
					if err != nil || len(list) == 0 {
						return "no backups yet", err
					}
					var b strings.Builder
					for _, a := range list {
						fmt.Fprintf(&b, "%s  %9s  %s\n", a.Timestamp, fmtBytes(float64(a.Size)), a.Path)
					}
					return b.String(), nil
				})
			}, covers: []string{"ipc:Backups"}},
		{id: "restore", group: "Services", title: "restore", about: "replace a service's storage from a backup",
			fields: []formField{svcField, optional("timestamp", "backup", "the newest")}, prefill: selectedService, needs: hasServiceOps,
			confirm: func(v vals) string {
				from := v["timestamp"]
				if from == "" {
					from = "its newest backup"
				}
				return fmt.Sprintf("Replace %s's storage with %s?\nAnything written since is lost.", v["service"], from)
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.serviceOps()
				return busy(m, "restoring "+v["service"], do("restore", v["service"], func() (string, error) {
					return x.Restore(v["service"], v["timestamp"])
				}))
			}, covers: []string{"ipc:Restore", "cli:restore"}},
		{id: "forget", group: "Services", title: "forget", about: "clear the record of a service whose manifest is gone",
			fields: []formField{svcField}, prefill: selectedService, needs: hasServiceOps,
			confirm: func(v vals) string { return "Stop " + v["service"] + " and clear its record?" },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.serviceOps()
				return m, do("forget", v["service"], func() (string, error) { return x.Forget(v["service"]) })
			}, covers: []string{"ipc:Forget"}},
		{id: "add", group: "Services", title: "add from GitHub", about: "install a service from its latest release: preview, then add",
			fields: []formField{text("repo", "repository", "owner/name or a github.com URL")}, needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				repo, r := v["repo"], m.runner
				return busy(m, "looking up "+repo, func() tea.Msg {
					out, err := r("add", "--print", repo)
					msg := outputMsg{title: "add " + repo, text: out, err: err}
					if err == nil {
						msg.thenKey, msg.thenLabel = "a", "to add it, esc to leave it"
						msg.then = func(m Model) (Model, tea.Cmd) {
							return busy(m, "adding "+repo, m.cli("add "+repo, "add", repo))
						}
					}
					return msg
				})
			}, covers: []string{"cli:add"}},
		{id: "add-binary", group: "Services", title: "add a local binary", about: "declare a service from a binary on this machine",
			fields: []formField{text("name", "name", "service name"), text("binary", "binary", "/path/to/binary"),
				optional("port", "port", "picked for you")}, needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				args := []string{"add", v["name"], "--binary", v["binary"]}
				if v["port"] != "" {
					args = append(args, "--port", v["port"])
				}
				return m, m.cli("add "+v["name"], args...)
			}, covers: []string{"cli:add"}},
		{id: "update", group: "Services", title: "update", about: "install a service's newer release, rolling back if it fails its healthcheck",
			fields: []formField{svcField}, prefill: selectedService, needs: hasRunner,
			confirm: func(v vals) string {
				return "Update " + v["service"] + "? It's backed up first, and restarts on the new release."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "updating "+v["service"], m.cli("update "+v["service"], "update", v["service"]))
			}, covers: []string{"cli:update"}},
		{id: "update-check", group: "Services", title: "check for updates", about: "whether any service has a newer release",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "checking releases", m.cli("updates", "update", "--check", "--all"))
			}, covers: []string{"cli:update"}},
		{id: "update-all", group: "Services", title: "update every service", about: "one at a time, stopping at the first that fails",
			needs: hasRunner, confirm: func(vals) string { return "Update every service with a newer release?" },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "updating", m.cli("update --all", "update", "--all"))
			}, covers: []string{"cli:update"}},
		{id: "remove", group: "Services", title: "remove", about: "undeclare a service; purge deletes its data and backups too",
			fields: []formField{svcField, choice("purge", "purge data", "no", "yes")}, prefill: selectedService, needs: hasRunner,
			confirm: func(v vals) string {
				if v["purge"] == "yes" {
					return "Remove " + v["service"] + " and permanently delete its storage and every backup of it?\nThis can't be undone."
				}
				return "Remove " + v["service"] + "? Its data stays where it is."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				args := []string{"remove", v["service"]}
				if v["purge"] == "yes" {
					// The confirmation was just given here; the command's own
					// prompt would read the dashboard's keyboard.
					args = append(args, "--purge", "--yes")
				}
				return m, m.cli("remove "+v["service"], args...)
			}, covers: []string{"cli:remove"}},

		// --- Users ---
		{id: "user-add", group: "Users", title: "add user", about: "a password is optional; without one they can't log in yet",
			fields: []formField{userField, optional("display", "display name", ""), optional("email", "email", ""),
				choice("admin", "admin", "no", "yes"), optional("groups", "groups", "comma separated"), secretField("password", "password", true)},
			needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				r := ipc.UserRequest{Username: v["user"], DisplayName: v["display"], Email: v["email"],
					Admin: v["admin"] == "yes", AddGroups: list(v["groups"]), Password: v["password"]}
				return m, do("add user", v["user"], func() (string, error) { return x.UserAdd(r) })
			}, covers: []string{"ipc:UserAdd", "cli:user add"}},
		{id: "user-passwd", group: "Users", title: "set password", about: "ends their sessions and revokes their access tokens",
			fields: []formField{userField, secretField("password", "password", false)}, prefill: selectedUserVals, needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("set password", v["user"], func() (string, error) { return x.UserPasswd(v["user"], v["password"]) })
			}, covers: []string{"ipc:UserPasswd", "cli:user passwd"}},
		{id: "user-groups", group: "Users", title: "change groups", about: "add to and remove from groups",
			fields:  []formField{userField, optional("add", "add", "comma separated"), optional("remove", "remove", "comma separated")},
			prefill: selectedUserVals, needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("groups", v["user"], func() (string, error) {
					return x.UserGroups(v["user"], list(v["add"]), list(v["remove"]))
				})
			}, covers: []string{"ipc:UserGroups", "cli:user groups"}},
		{id: "user-admin", group: "Users", title: "admin on/off", about: "grant or revoke admin",
			fields: []formField{userField, choice("admin", "admin", "on", "off")}, prefill: func(m Model) vals {
				v := selectedUserVals(m)
				if u, ok := m.selectedUserInfo(); ok && u.Admin {
					v["admin"] = "off"
				}
				return v
			}, needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("admin", v["user"], func() (string, error) { return x.UserAdmin(v["user"], v["admin"] == "on") })
			}, covers: []string{"ipc:UserAdmin", "cli:user admin"}},
		{id: "user-disable", group: "Users", title: "disable user", about: "block them from logging in and end their sessions",
			fields: []formField{userField}, prefill: selectedUserVals, needs: hasUserOps,
			confirm: func(v vals) string {
				return "Disable " + v["user"] + "? Their sessions end and their access tokens are revoked."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("disable", v["user"], func() (string, error) { return x.UserDisable(v["user"]) })
			}, covers: []string{"ipc:UserDisable", "cli:user disable"}},
		{id: "user-enable", group: "Users", title: "enable user", about: "let a disabled user log in again",
			fields: []formField{userField}, prefill: selectedUserVals, needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("enable", v["user"], func() (string, error) { return x.UserEnable(v["user"]) })
			}, covers: []string{"ipc:UserEnable", "cli:user enable"}},
		{id: "user-tailnet", group: "Users", title: "link tailnet login", about: "the Tailscale login that counts as them; blank unlinks",
			fields: []formField{userField, optional("login", "login", "blank to unlink")}, prefill: func(m Model) vals {
				v := selectedUserVals(m)
				if u, ok := m.selectedUserInfo(); ok {
					v["login"] = u.TailnetLogin
				}
				return v
			}, needs: hasUserOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("tailnet", v["user"], func() (string, error) { return x.UserTailnet(v["user"], v["login"]) })
			}, covers: []string{"ipc:UserTailnet"}},
		{id: "user-logout", group: "Users", title: "sign out everywhere", about: "end every session and access token they have",
			fields: []formField{userField}, prefill: selectedUserVals,
			needs:   func(m Model) bool { _, ok := m.extras(); return ok },
			confirm: func(v vals) string { return "Sign " + v["user"] + " out everywhere, on every device?" },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.signOut(v["user"])
			}, covers: []string{"ipc:UserLogout", "cli:user logout"}},
		{id: "user-rm", group: "Users", title: "remove user", about: "delete the account",
			fields: []formField{userField}, prefill: selectedUserVals, needs: hasUserOps,
			confirm: func(v vals) string { return "Remove " + v["user"] + "? Their account, sessions and tokens go." },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("remove user", v["user"], func() (string, error) { return x.UserRemove(v["user"]) })
			}, covers: []string{"ipc:UserRemove", "cli:user rm"}},
		{id: "users", group: "Users", title: "users and sign-ins", about: "everyone, and where each is signed in",
			needs: func(m Model) bool { _, ok := m.extras(); return ok },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewUsers)
			}, covers: []string{"ipc:UserList", "ipc:UserSignIns", "cli:user list", "cli:user sessions"}},
		{id: "rotate-key", group: "Users", title: "rotate signing key", about: "replace the key that signs identity tokens",
			needs: hasUserOps, confirm: func(vals) string {
				return "Replace the key that signs identity tokens? The old one stays published for a while."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.userOps()
				return m, do("rotate key", "", x.AuthRotateKey)
			}, covers: []string{"ipc:AuthRotateKey", "cli:auth rotate-key"}},

		// --- Tokens ---
		{id: "token-create", group: "Tokens", title: "create token", about: "an access token for scripts; the secret is shown once",
			fields: []formField{userField, optional("name", "name", ""), optional("apps", "apps", "every app they may reach"),
				optional("expires", "expires", "never, or 90d, 12w, 24h"), choice("internet", "internet", "no", "yes")},
			prefill: selectedUserVals, needs: hasTokenOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.tokenOps()
				return m, show("new token for "+v["user"], func() (string, error) {
					exp, err := parseExpiry(v["expires"])
					if err != nil {
						return "", err
					}
					secret, info, warn, err := x.TokenCreate(ipc.TokenRequest{Username: v["user"], Name: v["name"],
						Apps: list(v["apps"]), ExpiresIn: exp, Internet: v["internet"] == "yes"})
					if err != nil {
						return "", err
					}
					out := fmt.Sprintf("token %d for %s\n\n%s\n\nCopy it now: it isn't shown again.\n", info.ID, info.Username, secret)
					if warn != "" {
						out += "\n" + warn + "\n"
					}
					return out, nil
				})
			}, covers: []string{"ipc:TokenCreate", "cli:token create"}},
		{id: "tokens", group: "Tokens", title: "tokens and clients", about: "access tokens and connected OAuth clients",
			needs: hasTokenOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewTokens)
			}, covers: []string{"ipc:TokenList", "ipc:TokenClients", "cli:token list"}},
		{id: "token-revoke", group: "Tokens", title: "revoke token", about: "end an access token",
			fields: []formField{text("id", "token id", "number")}, prefill: func(m Model) vals {
				return vals{"id": m.selectedTokenID()}
			}, needs: hasTokenOps,
			confirm: func(v vals) string { return "Revoke access token " + v["id"] + "?" },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.tokenOps()
				return m, do("revoke", "token "+v["id"], func() (string, error) {
					id, err := strconv.ParseInt(v["id"], 10, 64)
					if err != nil {
						return "", errors.New("a token id is a number")
					}
					return x.TokenRevoke(id)
				})
			}, covers: []string{"ipc:TokenRevoke", "cli:token revoke"}},
		{id: "client-revoke", group: "Tokens", title: "disconnect client", about: "end an OAuth client's grant and its tokens",
			fields: []formField{text("id", "client id", "number")}, prefill: func(m Model) vals {
				return vals{"id": m.selectedClientID()}
			}, needs: hasTokenOps,
			confirm: func(v vals) string { return "Disconnect OAuth client " + v["id"] + "?" },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.tokenOps()
				return m, do("disconnect", "client "+v["id"], func() (string, error) {
					id, err := strconv.ParseInt(v["id"], 10, 64)
					if err != nil {
						return "", errors.New("a client id is a number")
					}
					return x.TokenRevokeClient(id)
				})
			}, covers: []string{"ipc:TokenRevokeClient"}},

		// --- Secrets ---
		{id: "secret-set", group: "Secrets", title: "set secret", about: "create one, or rotate its value; services that get it restart",
			fields: []formField{text("name", "name", "secret name"), optional("env", "variable", "NAME in capitals"),
				secretField("value", "value", false), optional("service", "give it to", "a service, or blank")},
			prefill: func(m Model) vals {
				if s, ok := m.selectedSecret(); ok {
					return vals{"name": s.Name, "env": s.Env}
				}
				return nil
			}, needs: hasSecretOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.secretOps()
				return m, do("set secret", v["name"], func() (string, error) {
					msg, err := x.SecretSet(v["name"], v["env"], v["value"])
					if err != nil || v["service"] == "" {
						return msg, err
					}
					more, err := x.SecretBind(v["name"], v["service"])
					return msg + "; " + more, err
				})
			}, covers: []string{"ipc:SecretSet", "cli:secret set"}},
		{id: "secret-bind", group: "Secrets", title: "give secret to", about: "a service, or every service with --global",
			fields:  []formField{text("name", "secret", "name"), text("service", "service", "a service, or --global")},
			prefill: func(m Model) vals { return vals{"name": m.selectedSecretName(), "service": m.selectedName()} }, needs: hasSecretOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.secretOps()
				return m, do("bind", v["name"], func() (string, error) { return x.SecretBind(v["name"], globalOr(v["service"])) })
			}, covers: []string{"ipc:SecretBind", "cli:secret bind"}},
		{id: "secret-unbind", group: "Secrets", title: "take secret from", about: "a service, or --global",
			fields:  []formField{text("name", "secret", "name"), text("service", "service", "a service, or --global")},
			prefill: func(m Model) vals { return vals{"name": m.selectedSecretName()} }, needs: hasSecretOps,
			confirm: func(v vals) string {
				return "Take " + v["name"] + " from " + v["service"] + "? It restarts without it."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.secretOps()
				return m, do("unbind", v["name"], func() (string, error) { return x.SecretUnbind(v["name"], globalOr(v["service"])) })
			}, covers: []string{"ipc:SecretUnbind", "cli:secret unbind"}},
		{id: "secret-rm", group: "Secrets", title: "delete secret", about: "services that get it restart without it",
			fields:  []formField{text("name", "secret", "name")},
			prefill: func(m Model) vals { return vals{"name": m.selectedSecretName()} }, needs: hasSecretOps,
			confirm: func(v vals) string { return "Delete secret " + v["name"] + "? Its value can't be got back." },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.secretOps()
				return m, do("delete secret", v["name"], func() (string, error) { return x.SecretRemove(v["name"]) })
			}, covers: []string{"ipc:SecretRemove", "cli:secret rm"}},
		{id: "secrets", group: "Secrets", title: "secrets", about: "every secret and what gets it, never a value",
			needs: hasSecretOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewSecrets)
			}, covers: []string{"ipc:SecretList", "cli:secret list"}},

		// --- AI ---
		{id: "ai", group: "AI", title: "models", about: "the models in force, the providers, which apps may use them",
			needs: func(m Model) bool { _, ok := m.aiClient(); return ok },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewAI)
			}, covers: []string{"ipc:AIStatus", "ipc:AISet", "ipc:AISetFrom", "ipc:AIReset", "ipc:AIApps", "ipc:AIAppsReset",
				"ipc:AIKey", "ipc:AITest", "cli:ai", "cli:ai set", "cli:ai reset", "cli:ai apps", "cli:ai key", "cli:ai test"}},
		{id: "ai-regenerate", group: "AI", title: "new AI token", about: "give an app a new OUTPOST_AI_TOKEN; it restarts",
			fields: []formField{svcField}, prefill: selectedService, needs: hasRegen,
			confirm: func(v vals) string { return "Give " + v["service"] + " a new AI token? It restarts." },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.client.(AIRegenerator)
				return m, do("regenerate", v["service"], func() (string, error) { return x.AIRegenerate(v["service"]) })
			}, covers: []string{"ipc:AIRegenerate", "cli:ai regenerate"}},

		// --- Gateway ---
		{id: "gateway", group: "Gateway", title: "gateway", about: "its addresses, the apps it routes, and its activity",
			needs: func(m Model) bool { _, ok := m.extras(); return ok },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewGateway)
			}, covers: []string{"ipc:GatewayStatus", "cli:gateway status", "cli:gateway activity"}},

		// --- System ---
		{id: "version", group: "System", title: "version", about: "this build, and the daemon's",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				r, dv := m.runner, m.daemonVersion
				return m, show("version", func() (string, error) {
					out, err := r("version")
					if dv != "" {
						out = strings.TrimRight(out, "\n") + "\n\ndaemon: " + dv + "\n"
					}
					return out, err
				})
			}, covers: []string{"cli:version", "ipc:DaemonVersion"}},
		{id: "config", group: "System", title: "config", about: "the global configuration",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.cli("config", "config")
			}, covers: []string{"cli:config", "ipc:DaemonConfig"}},
		{id: "config-init", group: "System", title: "create config", about: "write a config.toml with the defaults, if there's none",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.cli("config --init", "config", "--init")
			}, covers: []string{"cli:config"}},
		{id: "config-tailscale", group: "System", title: "tailscale on/off", about: "give services their own Tailscale identity, or not",
			fields: []formField{choice("on", "tailscale", "on", "off")}, needs: hasRunner,
			confirm: func(v vals) string { return "Turn Tailscale " + v["on"] + "? It takes a daemon restart to apply." },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.cli("config --tailscale "+v["on"], "config", "--tailscale", v["on"])
			}, covers: []string{"cli:config"}},
		{id: "spec", group: "System", title: "service contract", about: "what a managed service has to satisfy",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m, m.cli("spec", "spec")
			}, covers: []string{"cli:spec"}},
		{id: "daemon-restart", group: "System", title: "restart daemon", about: "through its autostart service, waiting for it to come back",
			needs: hasRunner, confirm: func(vals) string { return "Restart the daemon? Every service restarts with it." },
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "restarting the daemon", m.inTerminal("daemon restart", "daemon", "restart"))
			}, covers: []string{"cli:daemon restart"}},
		{id: "self-check", group: "System", title: "check for an Outpost update", about: "whether a newer Outpost is out",
			needs: hasRunner,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "checking", m.cli("update --self --check", "update", "--self", "--check"))
			}, covers: []string{"cli:update"}},
		{id: "self-update", group: "System", title: "update Outpost", about: "install the newest Outpost and restart the daemon onto it",
			needs: hasRunner, confirm: func(vals) string {
				return "Update Outpost? The daemon and every service restart onto the new build."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return busy(m, "updating Outpost", m.inTerminal("update --self", "update", "--self"))
			}, covers: []string{"cli:update"}},
	}
}

// globalOr turns the CLI's --global into the socket's empty service.
func globalOr(service string) string {
	if service == "--global" || service == "global" {
		return ""
	}
	return service
}

func orEvery(name string) string {
	if name == "" {
		return "every service"
	}
	return name
}

// Covers is everything the dashboard reaches, as "ipc:<Client method>" and
// "cli:<command>", whether through the palette or a view. The CLI's tests
// hold it to the commands in its help.
func Covers() map[string]bool {
	out := map[string]bool{
		// Not actions, but what the dashboard does on its own.
		"ipc:Available":  true, // every fetch asks it first
		"ipc:Stats":      true, // the graphs and the overview
		"ipc:Watch":      true, // the pushed service list
		"ipc:Identity":   true, // plumbing for daemon restart's wait, reached through it
		"cli:tui":        true, // this
		"cli:list":       true, // the services view
		"cli:logs":       true, // the logs view
		"cli:user list":  true, // the users view
		"cli:token list": true, // the tokens view
		"cli:help":       true, // the ? key
	}
	for _, a := range actions() {
		for _, c := range a.covers {
			out[c] = true
		}
	}
	return out
}
