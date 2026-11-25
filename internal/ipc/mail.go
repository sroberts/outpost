package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/mail"
)

// MailRequest is the argument to a mail.* command.
type MailRequest struct {
	// Service is the app a per-app command is about.
	Service string `json:"service,omitempty"`
	// Route is the alias mail.alias-rm removes, without the plus.
	Route string `json:"route,omitempty"`
	// Note is mail.alias-add's reminder of what the address is for.
	Note string `json:"note,omitempty"`
	// Send is mail.send's: whether the app may send as the account.
	Send bool `json:"send,omitempty"`
	// Address and Password are mail.account's Gmail login. The password is
	// an app password; like a secret's value it crosses the 0600 socket in
	// clear, and the server never logs a request.
	Address  string `json:"address,omitempty"`
	Password string `json:"password,omitempty"`
}

// MailOverview is mail.status's result: the account, and each app's
// addresses and whether it may send. Never the password.
type MailOverview struct {
	Account mail.Status `json:"account"`
	// Secret is where the app password is kept.
	Secret string    `json:"secret,omitempty"`
	Apps   []MailApp `json:"apps,omitempty"`
}

// MailApp is one app's mail.
type MailApp struct {
	Name   string       `json:"name"`
	Routes []mail.Route `json:"routes,omitempty"`
	Send   bool         `json:"send,omitempty"`
	// Held is how many messages wait for it, and the newest's arrival.
	Held       int       `json:"held,omitempty"`
	HeldNewest time.Time `json:"held_newest,omitzero"`
}

// mailCheckWait bounds waiting on Gmail for mail.account and mail.check,
// as the portal's Mail page does, inside the request's own budget.
const mailCheckWait = 20 * time.Second

// UseMail enables the mail.* commands. Call it before Serve.
func (s *Server) UseMail(m gateway.Mail) { s.mail = m }

// dispatchMail serves the mail.* commands. mail.db has its own locking,
// as the portal relies on, so nothing here touches the loop.
func (s *Server) dispatchMail(ctx context.Context, req Request) Response {
	if s.mail == nil {
		return errorResponse(errors.New("mail is not available: this daemon was started without it"))
	}
	if req.Command == CmdMailStatus {
		o, err := s.mailOverview(ctx)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Mail: &o}
	}
	if req.Command == CmdMailCheck {
		wait, cancel := context.WithTimeout(ctx, mailCheckWait)
		defer cancel()
		if err := s.mail.CheckWait(wait); err != nil {
			return Response{OK: true, Message: "still checking the inbox: Gmail is slow to answer; see `outpost mail` in a moment"}
		}
		st := s.mail.Status()
		if st.Error != "" {
			return errorResponse(fmt.Errorf("checked the inbox, and it failed: %s", st.Error))
		}
		return Response{OK: true, Message: "checked the inbox"}
	}
	r := req.Mail
	if r == nil {
		return errorResponse(errors.New("the request is missing its arguments"))
	}
	if req.Command == CmdMailAccount {
		if strings.TrimSpace(r.Address) == "" || r.Password == "" {
			return errorResponse(errors.New("an address and an app password are both needed"))
		}
		// Tried before it's saved, as the portal does, so a wrong password
		// is said now rather than at the next check.
		wait, cancel := context.WithTimeout(ctx, mailCheckWait)
		err := s.mail.TestAccount(wait, r.Address, r.Password)
		cancel()
		if err == nil {
			err = s.mail.SetAccount(r.Address, r.Password, cliUser)
		}
		if err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventMail, "set the mail account from the CLI")
		return Response{OK: true, Message: "saved, and Gmail took the login"}
	}

	// The rest are about one app, which has to be one the daemon knows.
	if r.Service == "" {
		return errorResponse(fmt.Errorf("%s: a service is required", req.Command))
	}
	if _, err := s.sup.Status(ctx, r.Service); err != nil {
		return errorResponse(err)
	}
	app := r.Service
	switch req.Command {
	case CmdMailAliasAdd:
		rt, err := s.mail.AddAlias(app, r.Note, cliUser)
		if err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventMail, "gave "+app+" the mail address +"+rt.Route+" from the CLI")
		msg := "made the address +" + rt.Route + " for " + app
		if rt.Address != "" {
			msg = "made the address " + rt.Address + "; mail to it reaches " + app + " from the next check"
		}
		return Response{OK: true, Message: msg, MailRoute: &rt}
	case CmdMailAliasRemove:
		if err := s.mail.RemoveAlias(app, r.Route, cliUser); err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventMail, "took the mail address +"+r.Route+" away from "+app+" from the CLI")
		return Response{OK: true, Message: "removed +" + r.Route + "; mail to it now stays in the inbox"}
	case CmdMailSend:
		if err := s.mail.SetSend(app, r.Send, cliUser); err != nil {
			return errorResponse(err)
		}
		if r.Send {
			s.feed(ctx, gateway.EventMail, "let "+app+" send mail from the CLI")
			return Response{OK: true, Message: app + " may send mail now, up to 100 messages a day"}
		}
		s.feed(ctx, gateway.EventMail, "stopped "+app+" sending mail from the CLI")
		return Response{OK: true, Message: app + " can't send mail any more"}
	case CmdMailRegenerate:
		if err := s.mail.Regenerate(app, cliUser); err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventMail, "regenerated "+app+"'s mail token from the CLI")
		return Response{OK: true, Message: "new mail token for " + app + "; it restarts with it, and the old one no longer works"}
	}
	return errorResponse(fmt.Errorf("unknown command %q", req.Command))
}

// mailOverview is the account and every declared app's mail.
func (s *Server) mailOverview(ctx context.Context) (MailOverview, error) {
	services, err := s.sup.List(ctx)
	if err != nil {
		return MailOverview{}, err
	}
	held, err := s.mail.Held()
	if err != nil {
		return MailOverview{}, err
	}
	o := MailOverview{Account: s.mail.Status(), Secret: s.mail.PasswordSecret()}
	for _, svc := range services {
		h := held[svc.Name]
		o.Apps = append(o.Apps, MailApp{Name: svc.Name, Routes: s.mail.Routes(svc.Name),
			Send: s.mail.CanSend(svc.Name), Held: h.Count, HeldNewest: h.Newest})
	}
	return o, nil
}

// MailStatus reads the mail account and every app's addresses.
func (c *Client) MailStatus() (MailOverview, error) {
	resp, err := c.do(Request{Command: CmdMailStatus})
	if err != nil {
		return MailOverview{}, err
	}
	if resp.Mail == nil {
		return MailOverview{}, errors.New("daemon returned no mail status")
	}
	return *resp.Mail, nil
}

// MailAccount sets the Gmail account mail comes through, once Gmail has
// taken the login.
func (c *Client) MailAccount(address, password string) (string, error) {
	resp, err := c.do(Request{Command: CmdMailAccount, Mail: &MailRequest{Address: address, Password: password}})
	return resp.Message, err
}

// MailCheck reads the inbox now, waiting a while for it to finish.
func (c *Client) MailCheck() (string, error) {
	resp, err := c.do(Request{Command: CmdMailCheck})
	return resp.Message, err
}

// MailAliasAdd makes service a new address, with note saying what it's for.
func (c *Client) MailAliasAdd(service, note string) (string, error) {
	resp, err := c.do(Request{Command: CmdMailAliasAdd, Mail: &MailRequest{Service: service, Note: note}})
	return resp.Message, err
}

// MailAliasRemove takes an address, its route without the plus, from service.
func (c *Client) MailAliasRemove(service, route string) (string, error) {
	resp, err := c.do(Request{Command: CmdMailAliasRemove, Mail: &MailRequest{Service: service, Route: strings.TrimPrefix(route, "+")}})
	return resp.Message, err
}

// MailSend says whether service may send mail as the account.
func (c *Client) MailSend(service string, allowed bool) (string, error) {
	resp, err := c.do(Request{Command: CmdMailSend, Mail: &MailRequest{Service: service, Send: allowed}})
	return resp.Message, err
}

// MailRegenerate gives service a new mail token; it restarts with it.
func (c *Client) MailRegenerate(service string) (string, error) {
	resp, err := c.do(Request{Command: CmdMailRegenerate, Mail: &MailRequest{Service: service}})
	return resp.Message, err
}
