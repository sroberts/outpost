// Package e2e drives a real Outpost daemon from a real browser: setup, the
// catalog, an app behind the login, an action from a Manage menu, and signing
// out, over HTTPS with the gateway's own certificate. The gateway's Go tests
// cover the same paths over HTTP; what only a browser shows is whether the
// cookie rules, the Content-Security-Policy and the pages' own scripts hold up
// together.
//
// It is its own module so chromedp never reaches the daemon's go.mod, and it
// treats Outpost as a black box: it builds the binary and talks to it the way
// a person would. Run it with `make e2e`. Without a Chrome or Chromium it
// skips, unless OUTPOST_E2E=1 says a browser must be there.
package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// domain is the parent of every host the browser is sent to. Chrome resolves
// it to loopback (--host-resolver-rules), so nothing leaves the machine.
const domain = "outpost.test"

const (
	adminName     = "admin"
	adminPassword = "correct horse battery staple"
)

func TestPortalInABrowser(t *testing.T) {
	chrome := findChrome(t)
	d := startDaemon(t)

	ctx, browserErrors := newBrowser(t, chrome)
	run := func(what string, actions ...chromedp.Action) {
		t.Helper()
		if err := chromedp.Run(ctx, actions...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// First run: the one-time link creates the first admin and signs them in.
	run("create the first admin",
		chromedp.Navigate(d.setupURL),
		chromedp.SendKeys(`input[name="username"]`, adminName, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="password"]`, adminPassword, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="confirm"]`, adminPassword, chromedp.ByQuery),
		chromedp.Submit(`form[action="/setup"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`li#app-echo.up a.name.go`, chromedp.ByQuery),
	)

	// The session cookie is host-only and can't be read by script or sent
	// cross-site: the __Host- prefix makes the browser itself enforce that.
	var cookies []*network.Cookie
	run("read cookies", chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{d.portal}).Do(ctx)
		return err
	}))
	session := findCookie(cookies, "__Host-")
	if session == nil {
		t.Fatalf("no __Host- session cookie on the portal; cookies: %s", cookieNames(cookies))
	}
	if !session.Secure || !session.HTTPOnly || session.Path != "/" || session.Domain != domain {
		t.Errorf("session cookie = %+v; want Secure, HttpOnly, Path=/ and host-only on %s", session, domain)
	}

	// The app is reached through the sign-in handoff, sees who the user is,
	// and never sees an Outpost cookie.
	var seenJSON string
	run("open the app from the catalog",
		chromedp.Click(`li#app-echo a.name.go`, chromedp.ByQuery),
		chromedp.WaitVisible(`#seen`, chromedp.ByQuery),
		chromedp.Text(`#seen`, &seenJSON, chromedp.ByQuery),
	)
	var seen struct {
		Headers map[string]string `json:"headers"`
		Cookies []string          `json:"cookies"`
	}
	if err := json.Unmarshal([]byte(seenJSON), &seen); err != nil {
		t.Fatalf("app page: %v\n%s", err, seenJSON)
	}
	if got := seen.Headers["X-Outpost-User"]; got != adminName {
		t.Errorf("the app saw X-Outpost-User %q; want %q (headers: %v)", got, adminName, seen.Headers)
	}
	if seen.Headers["X-Outpost-Identity"] == "" {
		t.Errorf("the app got no X-Outpost-Identity token (headers: %v)", seen.Headers)
	}
	for _, c := range seen.Cookies {
		if strings.Contains(strings.ToLower(c), "outpost") {
			t.Errorf("the app received the cookie %q", c)
		}
	}

	// frame-ancestors 'none': a page on another origin can't frame the portal.
	var frameURL string
	run("frame the portal from the app",
		chromedp.Navigate(d.appURL("echo")+"/frame?src="+d.portal+"/"),
		chromedp.WaitReady(`#portal`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.ActionFunc(func(ctx context.Context) error {
			tree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return err
			}
			for _, child := range tree.ChildFrames {
				frameURL = child.Frame.URL + child.Frame.UnreachableURL
			}
			return nil
		}),
	)
	if strings.HasPrefix(frameURL, d.portal) && !strings.HasPrefix(frameURL, "chrome-error:") {
		t.Errorf("the portal loaded inside another origin's frame (%s)", frameURL)
	}
	// That refusal is the one CSP report this test causes on purpose.
	browserErrors.forget()

	// An action from a Manage menu: the confirmation, the form's Origin check
	// and poll.js reloading the catalog until the app is back up.
	run("restart the app from its Manage menu",
		chromedp.Navigate(d.portal+"/"),
		chromedp.Click(`li#app-echo details.acts > summary`, chromedp.ByQuery),
		chromedp.Click(`li#app-echo details.confirm:has(input[value="restart"]) > summary`, chromedp.ByQuery),
		chromedp.Submit(`li#app-echo form:has(input[name="action"][value="restart"])`, chromedp.ByQuery),
	)
	waitRestarted(t, ctx, d)

	// Signing out ends the login everywhere: the app sends the browser back
	// to sign in.
	var location string
	run("sign out",
		chromedp.Navigate(d.portal+"/"),
		chromedp.Submit(`form[action="/logout"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`form[action="/login"]`, chromedp.ByQuery),
		chromedp.Navigate(d.appURL("echo")+"/"),
		chromedp.WaitVisible(`form[action="/login"]`, chromedp.ByQuery),
		chromedp.Location(&location),
	)
	if !strings.HasPrefix(location, d.portal+"/login") {
		t.Errorf("after signing out the app went to %s; want the portal's sign-in page", location)
	}

	if errs := browserErrors.all(); len(errs) > 0 {
		t.Errorf("the browser reported errors on Outpost's pages:\n%s", strings.Join(errs, "\n"))
	}
}

// waitRestarted waits for the restart to show in the catalog with no help:
// poll.js reloads the page while the row is busy, and the row must come back
// up, with the daemon counting one restart.
func waitRestarted(t *testing.T, ctx context.Context, d *daemon) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var up bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			`!!document.querySelector("li#app-echo.up:not(.busy)")`, &up)); err == nil && up && d.started("echo") >= 2 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the catalog didn't show echo back up after a restart; daemon log:\n%s", d.log())
}

// newBrowser starts a headless browser that resolves every host under domain
// to loopback and accepts the daemon's self-signed certificate.
func newBrowser(t *testing.T, chrome string) (context.Context, *errorLog) {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.Flag("host-resolver-rules", "MAP "+domain+" 127.0.0.1, MAP *."+domain+" 127.0.0.1"),
		chromedp.Flag("ignore-certificate-errors", true),
		// The daemon is on loopback; a proxy from the environment would
		// either refuse it or resolve outpost.test on its own.
		chromedp.ProxyServer("direct://"),
		chromedp.Flag("proxy-bypass-list", "*"),
	)
	if os.Getuid() == 0 {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	ctx, cancelTimeout := context.WithTimeout(ctx, 2*time.Minute)
	t.Cleanup(func() { cancelTimeout(); cancelCtx(); cancelAlloc() })

	errs := &errorLog{}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			errs.add("exception: " + ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				var parts []string
				for _, a := range ev.Args {
					parts = append(parts, string(a.Value))
				}
				errs.add("console.error: " + strings.Join(parts, " "))
			}
		case *cdplog.EventEntryAdded:
			// CSP violations arrive here, as do failed loads.
			if ev.Entry.Level == cdplog.LevelError {
				errs.add(fmt.Sprintf("%s: %s (%s)", ev.Entry.Source, ev.Entry.Text, ev.Entry.URL))
			}
		}
	})
	if err := chromedp.Run(ctx, cdplog.Enable(), runtime.Enable()); err != nil {
		t.Fatalf("start the browser: %v", err)
	}
	return ctx, errs
}

// errorLog collects what the browser reports going wrong.
type errorLog struct {
	mu   sync.Mutex
	errs []string
}

func (l *errorLog) add(s string) { l.mu.Lock(); l.errs = append(l.errs, s); l.mu.Unlock() }
func (l *errorLog) forget()      { l.mu.Lock(); l.errs = nil; l.mu.Unlock() }
func (l *errorLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.errs...)
}

// findChrome finds a browser for chromedp, or skips the test when there is
// none and nobody asked for one.
func findChrome(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OUTPOST_E2E_CHROME"); p != "" {
		return p
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// Playwright's own Chromium, where it's installed.
	if m, _ := filepath.Glob("/opt/pw-browsers/chromium-*/chrome-linux*/chrome"); len(m) > 0 {
		return m[len(m)-1]
	}
	if os.Getenv("OUTPOST_E2E") == "1" {
		t.Fatal("OUTPOST_E2E=1 but no Chrome or Chromium was found; set OUTPOST_E2E_CHROME")
	}
	t.Skip("no Chrome or Chromium found; set OUTPOST_E2E_CHROME to run the browser test")
	return ""
}

// daemon is an `outpost serve` running in a fresh home with the gateway on
// and Tailscale off.
type daemon struct {
	home, bin string
	port      int
	portal    string
	setupURL  string

	mu  sync.Mutex
	out strings.Builder
}

func (d *daemon) appURL(name string) string {
	return fmt.Sprintf("https://%s.%s:%d", name, domain, d.port)
}

func (d *daemon) log() string { d.mu.Lock(); defer d.mu.Unlock(); return d.out.String() }

// started counts how often the daemon has said it started name.
func (d *daemon) started(name string) int {
	return len(regexp.MustCompile(`(?m)^started `+regexp.QuoteMeta(name)+` `).FindAllString(d.log(), -1))
}

func startDaemon(t *testing.T) *daemon {
	t.Helper()
	// Under os.TempDir, not t.TempDir: the control socket's path has to fit
	// darwin's 104 bytes.
	home, err := os.MkdirTemp("", "ope2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a leftover temp dir is harmless
	bin := filepath.Join(home, "bin")
	outpost := build(t, bin, "outpost", "github.com/sroberts/outpost/cmd/outpost", "..")
	echo := build(t, bin, "echoapp", "./echoapp", ".")

	d := &daemon{home: home, bin: bin, port: freePort(t)}
	d.portal = fmt.Sprintf("https://%s:%d", domain, d.port)
	cert, key := writeCert(t, home)
	appPort := freePort(t)

	must(t, os.MkdirAll(filepath.Join(home, "services"), 0o700))
	must(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[tailscale]
enabled = false

[gateway]
enabled = true
listen = "127.0.0.1:%d"
public_url = %q
tls_cert = %q
tls_key = %q
`, d.port, d.portal, cert, key)), 0o600))
	must(t, os.WriteFile(filepath.Join(home, "services", "echo.toml"), []byte(fmt.Sprintf(`
name = "echo"
runtime = "process"
binary = %q
args = ["-port", "%d"]
port = %d
storage = %q
healthcheck = "/up"
`, echo, appPort, appPort, filepath.Join(home, "data", "echo"))), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, outpost, "--home", home, "serve")
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	cmd.Stderr = cmd.Stdout
	must(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait() // the daemon exits non-zero on a signal; its log is what matters
		if t.Failed() {
			t.Logf("daemon log:\n%s", d.log())
		}
	})

	setupRe := regexp.MustCompile(`create the first admin at (\S+)`)
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		var once sync.Once
		for sc.Scan() {
			line := sc.Text()
			d.mu.Lock()
			d.out.WriteString(line + "\n")
			if m := setupRe.FindStringSubmatch(line); m != nil {
				d.setupURL = m[1]
			}
			up := d.setupURL != "" && strings.HasPrefix(line, "started echo ")
			d.mu.Unlock()
			if up {
				once.Do(func() { close(ready) })
			}
		}
		_, _ = io.Copy(io.Discard, stdout) // drain whatever is left so the daemon never blocks on a write
	}()
	select {
	case <-ready:
	case <-time.After(60 * time.Second):
		t.Fatalf("the daemon didn't print a setup link and start echo within a minute:\n%s", d.log())
	}
	return d
}

// build compiles pkg from dir into bin and returns the binary's path.
func build(t *testing.T, bin, name, pkg, dir string) string {
	t.Helper()
	out := filepath.Join(bin, name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// writeCert writes a self-signed certificate for domain and every host under
// it, as the gateway's tls_cert and tls_key.
func writeCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain, "*." + domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	must(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	must(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func findCookie(cs []*network.Cookie, prefix string) *network.Cookie {
	for _, c := range cs {
		if strings.HasPrefix(c.Name, prefix) {
			return c
		}
	}
	return nil
}

func cookieNames(cs []*network.Cookie) string {
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
