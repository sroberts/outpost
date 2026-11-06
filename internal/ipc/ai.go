package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/gateway"
)

// AIRequest is the argument to an ai.* command.
type AIRequest struct {
	Kind  string `json:"kind,omitempty"`
	Model string `json:"model,omitempty"`
	// Provider is ai.set's provider, "" for the one in force, and ai.key's.
	Provider string `json:"provider,omitempty"`
	// Service is the app ai.regenerate gives a new token.
	Service string `json:"service,omitempty"`
	// Every and Apps are ai.apps': every app, or those named.
	Every bool     `json:"every,omitempty"`
	Apps  []string `json:"apps,omitempty"`
	// Value is ai.key's API key. Like a secret's, it crosses the 0600 socket
	// in clear, and the server never logs a request.
	Value string `json:"value,omitempty"`
}

// UseAI enables the ai.* commands. Call it before Serve.
func (s *Server) UseAI(m gateway.AIModels) { s.ai = m }

// cliUser is who the activity feed and the AI log say made a change from the
// CLI: whoever can reach the 0600 socket, which is the operator.
const cliUser = "the CLI"

// dispatchAI serves the ai.* commands. ai.db has its own locking and the
// endpoint reads it on each request, so nothing here touches the loop.
func (s *Server) dispatchAI(ctx context.Context, req Request) Response {
	if s.ai == nil {
		return errorResponse(errors.New("AI models are not available: this daemon was started without them"))
	}
	if req.Command == CmdAIStatus {
		o := s.ai.Overview(ctx)
		return Response{OK: true, AI: &o}
	}
	r := req.AI
	if r == nil {
		return errorResponse(errors.New("the request is missing its arguments"))
	}
	controls, hosted := s.ai.(gateway.AIControls)
	if !hosted && (r.Provider != "" || req.Command == CmdAIApps || req.Command == CmdAIAppsReset || req.Command == CmdAIKey) {
		return errorResponse(errors.New("this daemon's AI models are Ollama's alone; update Outpost for hosted providers and app access"))
	}
	var msg string
	switch req.Command {
	case CmdAISet:
		var saved string
		var err error
		if hosted {
			saved, err = controls.SetModel(ctx, r.Kind, r.Provider, r.Model, cliUser)
		} else {
			saved, err = s.ai.Set(ctx, r.Kind, r.Model, cliUser)
		}
		if err != nil {
			return errorResponse(err)
		}
		what := saved
		if r.Provider != "" {
			what = ai.ProviderTitle(r.Provider) + "'s " + saved
		}
		msg = fmt.Sprintf("The %s is now %s. Apps get it from their next request; none of them restarts.", ai.KindName(r.Kind), what)
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("the %s set to %s from the CLI", ai.KindName(r.Kind), what))
	case CmdAIApps:
		if err := controls.SetApps(r.Kind, r.Every, r.Apps, cliUser); err != nil {
			return errorResponse(err)
		}
		who := "every app"
		if !r.Every {
			who = "only " + strings.Join(r.Apps, ", ")
			if len(r.Apps) == 0 {
				who = "no app"
			}
		}
		msg = fmt.Sprintf("The %s is now for %s, from the next request.", ai.KindName(r.Kind), who)
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("the %s given to %s from the CLI", ai.KindName(r.Kind), who))
	case CmdAIAppsReset:
		if err := controls.ResetApps(r.Kind, cliUser); err != nil {
			return errorResponse(err)
		}
		msg = fmt.Sprintf("The %s's apps are back to config.toml's.", ai.KindName(r.Kind))
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("the %s's apps put back to config.toml's from the CLI", ai.KindName(r.Kind)))
	case CmdAIKey:
		if err := controls.SetKey(r.Provider, r.Value, cliUser); err != nil {
			return errorResponse(err)
		}
		msg = fmt.Sprintf("Saved %s's API key. No app is given it; Outpost sends it with each request.", ai.ProviderTitle(r.Provider))
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("%s's API key set from the CLI", ai.ProviderTitle(r.Provider)))
	case CmdAIReset:
		if err := s.ai.Reset(r.Kind, cliUser); err != nil {
			return errorResponse(err)
		}
		msg = fmt.Sprintf("The %s is back to config.toml's.", ai.KindName(r.Kind))
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("the %s put back to config.toml's from the CLI", ai.KindName(r.Kind)))
	case CmdAITest:
		res, err := s.ai.Test(ctx, r.Kind)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, AITest: &res}
	case CmdAIRegenerate:
		if r.Service == "" {
			return errorResponse(errors.New("name the app to give a new token"))
		}
		if !s.declared(ctx, r.Service) {
			return errorResponse(fmt.Errorf("no service named %s", r.Service))
		}
		if err := s.ai.Regenerate(r.Service, cliUser); err != nil {
			return errorResponse(err)
		}
		msg = fmt.Sprintf("%s has a new AI token and restarts with it; the old one no longer works.", r.Service)
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("%s's AI token regenerated from the CLI", r.Service))
	default:
		return errorResponse(fmt.Errorf("unknown command %q", req.Command))
	}
	return Response{OK: true, Message: msg}
}

// declared reports whether service is one the daemon supervises.
func (s *Server) declared(ctx context.Context, service string) bool {
	for _, name := range s.serviceNames(ctx) {
		if name == service {
			return true
		}
	}
	return false
}

// AIStatus reports the models in force and what Ollama has.
func (c *Client) AIStatus() (ai.Overview, error) {
	resp, err := c.do(Request{Command: CmdAIStatus})
	if err != nil {
		return ai.Overview{}, err
	}
	if resp.AI == nil {
		return ai.Overview{}, errors.New("daemon returned no AI status")
	}
	return *resp.AI, nil
}

// AISet chooses the model for kind, over config.toml's.
func (c *Client) AISet(kind, model string) (string, error) {
	return c.AISetFrom(kind, "", model)
}

// AISetFrom chooses provider's model for kind; "" is the provider in force.
func (c *Client) AISetFrom(kind, provider, model string) (string, error) {
	resp, err := c.do(Request{Command: CmdAISet, AI: &AIRequest{Kind: kind, Provider: provider, Model: model}})
	return resp.Message, err
}

// AIApps chooses which apps may use kind: every one, or those named.
func (c *Client) AIApps(kind string, every bool, apps []string) (string, error) {
	resp, err := c.do(Request{Command: CmdAIApps, AI: &AIRequest{Kind: kind, Every: every, Apps: apps}})
	return resp.Message, err
}

// AIAppsReset goes back to config.toml's apps for kind.
func (c *Client) AIAppsReset(kind string) (string, error) {
	resp, err := c.do(Request{Command: CmdAIAppsReset, AI: &AIRequest{Kind: kind}})
	return resp.Message, err
}

// AIKey saves a hosted provider's API key.
func (c *Client) AIKey(provider, value string) (string, error) {
	resp, err := c.do(Request{Command: CmdAIKey, AI: &AIRequest{Provider: provider, Value: value}})
	return resp.Message, err
}

// AIReset goes back to config.toml's model for kind.
func (c *Client) AIReset(kind string) (string, error) {
	resp, err := c.do(Request{Command: CmdAIReset, AI: &AIRequest{Kind: kind}})
	return resp.Message, err
}

// AITest sends the model for kind one small request.
func (c *Client) AITest(kind string) (ai.TestResult, error) {
	resp, err := c.do(Request{Command: CmdAITest, AI: &AIRequest{Kind: kind}})
	if err != nil {
		return ai.TestResult{}, err
	}
	if resp.AITest == nil {
		return ai.TestResult{}, errors.New("daemon returned no test result")
	}
	return *resp.AITest, nil
}

// AIRegenerate gives service a new AI token.
func (c *Client) AIRegenerate(service string) (string, error) {
	resp, err := c.do(Request{Command: CmdAIRegenerate, AI: &AIRequest{Service: service}})
	return resp.Message, err
}
