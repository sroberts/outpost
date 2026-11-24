package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sroberts/outpost/internal/gateway"
)

// ExposureRequest is the argument to an exposure.* command. What a service
// exposes now is in its ServiceStatus; these change it.
type ExposureRequest struct {
	Service string `json:"service"`
	// Funnel and Paths are exposure.set's: whether the internet reaches the
	// service over Funnel, and the paths that skip the login.
	Funnel bool     `json:"funnel,omitempty"`
	Paths  []string `json:"paths,omitempty"`
	// On is exposure.mcp's: whether the internet reaches the MCP endpoint.
	On bool `json:"on,omitempty"`
}

// UseExposure enables the exposure.* commands. Call it before Serve. e is
// what the portal sets an app's internet access through, so the socket
// holds a setting to the same manifest rules and applies it the same way.
func (s *Server) UseExposure(e gateway.Exposure) { s.exposure = e }

// dispatchExposure serves the exposure.* commands. Setting one rewrites the
// service's exposure and reapplies, which goes through the supervisor's own
// API and so its loop.
func (s *Server) dispatchExposure(ctx context.Context, req Request) Response {
	if s.exposure == nil {
		return errorResponse(errors.New("internet access can't be set here: this daemon was started without it"))
	}
	r := req.Exposure
	if r == nil || r.Service == "" {
		return errorResponse(fmt.Errorf("%s: a service is required", req.Command))
	}
	app := r.Service
	switch req.Command {
	case CmdExposureSet:
		if err := s.exposure.Set(ctx, app, r.Funnel, r.Paths, cliUser); err != nil {
			return errorResponse(err)
		}
		what := describeExposure(r.Funnel, r.Paths)
		s.feed(ctx, gateway.EventExposure, what+" for "+app+" from the CLI")
		return Response{OK: true, Message: what + " for " + app}
	case CmdExposureReset:
		if err := s.exposure.Reset(ctx, app, cliUser); err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventExposure, "went back to the manifest's public paths and Funnel for "+app+" from the CLI")
		return Response{OK: true, Message: app + " has its manifest's public paths and Funnel again"}
	}
	m, ok := s.exposure.(gateway.MCPExposure)
	if !ok {
		return errorResponse(errors.New("this daemon can't set an MCP endpoint's internet access"))
	}
	switch req.Command {
	case CmdExposureMCP:
		if err := m.SetMCPFunnel(ctx, app, r.On, cliUser); err != nil {
			return errorResponse(err)
		}
		what := "took " + app + "'s MCP endpoint off the internet"
		if r.On {
			what = "put " + app + "'s MCP endpoint on the internet"
		}
		s.feed(ctx, gateway.EventExposure, what+" from the CLI")
		return Response{OK: true, Message: what}
	case CmdExposureMCPReset:
		if err := m.ResetMCPFunnel(ctx, app, cliUser); err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventExposure, "went back to the manifest's setting for "+app+"'s MCP endpoint on the internet from the CLI")
		return Response{OK: true, Message: app + "'s MCP endpoint is as its manifest says again"}
	}
	return errorResponse(fmt.Errorf("unknown command %q", req.Command))
}

// describeExposure says what exposure.set did, in the portal's words.
func describeExposure(funnel bool, paths []string) string {
	what := "set public paths to none"
	if len(paths) > 0 {
		what = "set public paths " + strings.Join(paths, ", ")
	}
	if funnel {
		return what + ", with Funnel on"
	}
	return what + ", with Funnel off"
}

// ExposureSet sets which of service's paths skip the login, and whether
// Funnel puts them on the internet, over what its manifest says.
func (c *Client) ExposureSet(service string, funnel bool, paths []string) (string, error) {
	resp, err := c.do(Request{Command: CmdExposureSet, Exposure: &ExposureRequest{Service: service, Funnel: funnel, Paths: paths}})
	return resp.Message, err
}

// ExposureReset goes back to the manifest's public paths and Funnel.
func (c *Client) ExposureReset(service string) (string, error) {
	resp, err := c.do(Request{Command: CmdExposureReset, Exposure: &ExposureRequest{Service: service}})
	return resp.Message, err
}

// ExposureMCP sets whether the internet reaches service's MCP endpoint,
// for access tokens made for the internet.
func (c *Client) ExposureMCP(service string, on bool) (string, error) {
	resp, err := c.do(Request{Command: CmdExposureMCP, Exposure: &ExposureRequest{Service: service, On: on}})
	return resp.Message, err
}

// ExposureMCPReset goes back to the manifest's mcp_funnel.
func (c *Client) ExposureMCPReset(service string) (string, error) {
	resp, err := c.do(Request{Command: CmdExposureMCPReset, Exposure: &ExposureRequest{Service: service}})
	return resp.Message, err
}
