package push

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxRequest bounds what an app sends: one notification and who it's for.
const maxRequest = 64 << 10

// maxRecipients bounds the users one send names.
const maxRecipients = 500

// sendRequest is the body of POST /v1/send.
type sendRequest struct {
	// To are usernames; none means everyone who can open the app.
	To []string `json:"to"`
	Notification
}

// ServeHTTP is the endpoint apps call.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	service, ok := s.serviceFor(appToken(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "this endpoint takes the token Outpost gives each app, in OUTPOST_PUSH_TOKEN")
		return
	}
	switch r.URL.Path {
	case "/v1/send":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		s.serveSend(w, r, service)
	default:
		writeError(w, http.StatusNotFound, "Outpost serves POST /v1/send")
	}
}

func (s *Service) serveSend(w http.ResponseWriter, r *http.Request, service string) {
	var req sendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the body has to be a JSON notification: "+err.Error())
		return
	}
	if len(req.To) > maxRecipients {
		writeError(w, http.StatusBadRequest, "name at most 500 users in to, or none for everyone who can open you")
		return
	}
	n, err := s.Send(service, req.To, req.Notification)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, struct {
			Devices int `json:"devices"`
		}{n})
	case IsBadRequest(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrLimit):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.logf("push: send for %s: %v", service, err)
		writeError(w, http.StatusInternalServerError, "couldn't queue the notification")
	}
}

// appToken is the token an app sent, as a bearer token.
func appToken(r *http.Request) string {
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the app hung up; nothing to tell it
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

// Serve answers apps on ln until ctx ends.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	var inFlight sync.WaitGroup
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			defer inFlight.Done()
			s.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), s.drain)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			_ = srv.Close() // closing anyway; its error says nothing more
		}
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Handlers use push.db, which closes after this returns.
	<-stopped
	inFlight.Wait()
	return nil
}
