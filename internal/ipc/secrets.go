package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/secrets"
)

// SecretRequest is the argument to a secret.* command. Value is in clear: it
// crosses the 0600 socket, as a new user's password does, and the server
// never logs a request.
type SecretRequest struct {
	Name  string `json:"name"`
	Env   string `json:"env,omitempty"`
	Value string `json:"value,omitempty"`
	// Service is what secret.bind and secret.unbind act on; "" with Global
	// set means every service.
	Service string `json:"service,omitempty"`
	Global  bool   `json:"global,omitempty"`
}

// SecretInfo is a secret as the CLI sees one. It has no value field, so
// nothing can read a value back out of the daemon.
type SecretInfo struct {
	Name      string    `json:"name"`
	Env       string    `json:"env"`
	Global    bool      `json:"global,omitempty"`
	Services  []string  `json:"services,omitempty"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Reaches are the declared services that get it now.
	Reaches []string `json:"reaches,omitempty"`
}

// UseSecrets lets the server manage secrets.
func (s *Server) UseSecrets(st *secrets.Store) { s.secrets = st }

var errNoSecrets = errors.New("secrets are not available: this daemon was started without a secrets store")

// dispatchSecret serves the secret.* commands. Like auth.db, secrets.db is
// its own database with its own locking; the supervisor notices a change on
// its next tick by the revision Resolve reports, so nothing here touches the
// loop.
func (s *Server) dispatchSecret(ctx context.Context, req Request) Response {
	if s.secrets == nil {
		return errorResponse(errNoSecrets)
	}
	names := s.serviceNames(ctx)
	if req.Command == CmdSecretList {
		list := s.secrets.List()
		infos := make([]SecretInfo, 0, len(list))
		for _, sec := range list {
			infos = append(infos, s.secretInfo(sec, names))
		}
		return Response{OK: true, Secrets: infos}
	}

	r := req.Secret
	if r == nil || r.Name == "" {
		return errorResponse(errors.New("a secret name is required"))
	}
	// Who loses it is only knowable before it goes.
	before := s.secrets.Reaches(r.Name, names)
	var (
		sec     secrets.Secret
		err     error
		verb    string
		removed bool
	)
	switch req.Command {
	case CmdSecretSet:
		if existing, ok := s.secrets.Get(r.Name); ok {
			if r.Env != "" && r.Env != existing.Env {
				return errorResponse(fmt.Errorf("secret %s is handed over as %s; to use another variable, remove it and set it again", r.Name, existing.Env))
			}
			sec, err = s.secrets.Rotate(r.Name, r.Value)
			verb = "rotated"
		} else {
			env := r.Env
			if env == "" {
				env = secrets.DefaultEnv(r.Name)
			}
			sec, err = s.secrets.Create(r.Name, env, r.Value)
			verb = "created"
		}
	case CmdSecretRemove:
		sec, err = s.secrets.Delete(r.Name)
		verb, removed = "removed", true
	case CmdSecretBind, CmdSecretUnbind:
		if r.Global == (r.Service != "") {
			return errorResponse(errors.New("name a service, or every service with --global"))
		}
		if req.Command == CmdSecretBind {
			sec, err = s.secrets.Bind(r.Name, r.Service)
			verb = "bound to " + orEvery(r.Service)
		} else {
			sec, err = s.secrets.Unbind(r.Name, r.Service)
			verb, removed = "unbound from "+orEvery(r.Service), true
		}
	default:
		return errorResponse(fmt.Errorf("unknown command %q", req.Command))
	}
	if err != nil {
		return errorResponse(err)
	}

	s.feed(ctx, gateway.EventSecret, fmt.Sprintf("secret %s %s from the CLI", sec.Name, verb))
	msg := fmt.Sprintf("secret %s (%s) %s", sec.Name, sec.Env, verb)
	if removed {
		if lost := without(before, s.secrets.Reaches(sec.Name, names)); len(lost) > 0 {
			msg += "; restarting " + strings.Join(lost, ", ") + " without it"
		}
	} else if now := s.secrets.Reaches(sec.Name, names); len(now) > 0 {
		msg += "; restarting " + strings.Join(now, ", ") + " with it"
	}
	return Response{OK: true, Message: msg, Secrets: []SecretInfo{s.secretInfo(sec, names)}}
}

// without is a less b.
func without(a, b []string) []string {
	drop := map[string]bool{}
	for _, x := range b {
		drop[x] = true
	}
	var out []string
	for _, x := range a {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) secretInfo(sec secrets.Secret, names []string) SecretInfo {
	return SecretInfo{
		Name: sec.Name, Env: sec.Env, Global: sec.Global, Services: sec.Services,
		Revision: sec.Revision, CreatedAt: sec.CreatedAt, UpdatedAt: sec.UpdatedAt,
		Reaches: s.secrets.Reaches(sec.Name, names),
	}
}

// serviceNames are the declared services, for saying who a change reaches.
// A failure only costs that sentence.
func (s *Server) serviceNames(ctx context.Context) []string {
	sts, err := s.sup.List(ctx)
	if err != nil {
		s.logf("outpost: list services for secrets: %v", err)
		return nil
	}
	out := make([]string, 0, len(sts))
	for _, st := range sts {
		out = append(out, st.Name)
	}
	return out
}

func orEvery(service string) string {
	if service == "" {
		return "every service"
	}
	return service
}
