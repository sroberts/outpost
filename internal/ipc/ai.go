package ipc

import (
	"context"
	"errors"
	"fmt"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/gateway"
)

// AIRequest is the argument to an ai.* command.
type AIRequest struct {
	Kind  string `json:"kind,omitempty"`
	Model string `json:"model,omitempty"`
	// Service is the app ai.regenerate gives a new token.
	Service string `json:"service,omitempty"`
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
	var msg string
	switch req.Command {
	case CmdAISet:
		saved, err := s.ai.Set(ctx, r.Kind, r.Model, cliUser)
		if err != nil {
			return errorResponse(err)
		}
		msg = fmt.Sprintf("The %s is now %s. Apps get it from their next request; none of them restarts.", ai.KindName(r.Kind), saved)
		s.feed(ctx, gateway.EventAI, fmt.Sprintf("the %s set to %s from the CLI", ai.KindName(r.Kind), saved))
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
	resp, err := c.do(Request{Command: CmdAISet, AI: &AIRequest{Kind: kind, Model: model}})
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
