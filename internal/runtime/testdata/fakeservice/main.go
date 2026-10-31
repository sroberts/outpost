// Command fakeservice is a minimal stand-in for an Outpost-managed service,
// used by internal/runtime's integration tests. It satisfies the §2 service
// contract (listens on one TCP port, serves a healthcheck) and can be told to
// misbehave on demand so restart policy and log capture can be exercised
// against a real process rather than a mock.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		port         = flag.Int("port", 0, "TCP port to listen on")
		health       = flag.String("health", "/up", "healthcheck path")
		healthyAfter = flag.Duration("healthy-after", 0, "delay before the healthcheck starts returning 2xx")
		unhealthyAt  = flag.Duration("unhealthy-after", 0, "start failing the healthcheck after this long (0 = never)")
		crashAfter   = flag.Duration("crash-after", 0, "exit with -exit-code after this long (0 = never)")
		exitCode     = flag.Int("exit-code", 1, "exit code used by -crash-after and /crash")
		ignoreTerm   = flag.Bool("ignore-term", false, "ignore SIGTERM, forcing the supervisor to escalate to SIGKILL")
	)
	flag.Parse()

	started := time.Now()
	var forcedUnhealthy atomic.Bool

	if *ignoreTerm {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
	}

	// Announce identity on stdout/stderr so log capture is verifiable.
	fmt.Printf("fakeservice: listening on port %d storage=%s\n", *port, os.Getenv("OUTPOST_STORAGE_DIR"))
	fmt.Fprintf(os.Stderr, "fakeservice: stderr stream open\n")

	mux := http.NewServeMux()
	mux.HandleFunc(*health, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case forcedUnhealthy.Load():
			http.Error(w, "forced unhealthy", http.StatusInternalServerError)
		case time.Since(started) < *healthyAfter:
			http.Error(w, "still starting", http.StatusServiceUnavailable)
		case *unhealthyAt > 0 && time.Since(started) >= *unhealthyAt:
			http.Error(w, "gone unhealthy", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
		}
	})
	// Test hooks: flip health, or exit non-zero, on request.
	mux.HandleFunc("/unhealthy", func(w http.ResponseWriter, r *http.Request) {
		forcedUnhealthy.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "fakeservice: crashing on request")
			os.Exit(*exitCode)
		}()
	})
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv(r.URL.Query().Get("k")))
	})
	mux.HandleFunc("/cwd", func(w http.ResponseWriter, r *http.Request) {
		dir, err := os.Getwd()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, dir)
	})

	if *crashAfter > 0 {
		go func() {
			time.Sleep(*crashAfter)
			fmt.Fprintln(os.Stderr, "fakeservice: crash-after elapsed")
			os.Exit(*exitCode)
		}()
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeservice: listen: %v\n", err)
		os.Exit(2)
	}
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "fakeservice: serve: %v\n", err)
		os.Exit(2)
	}
}
