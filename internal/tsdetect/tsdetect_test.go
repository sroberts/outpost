package tsdetect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fake builds a detector with the CLI at cliPath (or nowhere when empty),
// answering status with out and err.
func fake(cliPath string, out string, err error) (detector, *[]string) {
	var ran []string
	return detector{
		lookPath: func(string) (string, error) {
			if cliPath == "" || cliPath == macAppCLI {
				return "", errors.New("not on PATH")
			}
			return cliPath, nil
		},
		exists: func(p string) bool { return cliPath == macAppCLI && p == macAppCLI },
		run: func(_ context.Context, path string, args ...string) ([]byte, error) {
			ran = append(ran, path)
			return []byte(out), err
		},
	}, &ran
}

const running = `{"BackendState":"Running","MagicDNSSuffix":"tail1234.ts.net",
"CurrentTailnet":{"Name":"alice@example.com","MagicDNSSuffix":"tail1234.ts.net","MagicDNSEnabled":true}}`

func TestDetect_Running(t *testing.T) {
	d, ran := fake("/usr/local/bin/tailscale", running, nil)
	got := d.detect(context.Background())
	if !got.Found || got.Tailnet != "tail1234.ts.net" {
		t.Errorf("detect = %+v; want found on tail1234.ts.net", got)
	}
	if len(*ran) != 1 || (*ran)[0] != "/usr/local/bin/tailscale" {
		t.Errorf("ran %v", *ran)
	}
}

func TestDetect_MacAppPath(t *testing.T) {
	d, ran := fake(macAppCLI, running, nil)
	if got := d.detect(context.Background()); !got.Found {
		t.Errorf("detect = %+v; want found via the app bundle", got)
	}
	if len(*ran) != 1 || (*ran)[0] != macAppCLI {
		t.Errorf("ran %v; want the app bundle's CLI", *ran)
	}
}

func TestDetect_NotFound(t *testing.T) {
	cases := map[string]detector{}
	cases["no cli"], _ = fake("", running, nil)
	cases["stopped"], _ = fake("/bin/tailscale", `{"BackendState":"Stopped"}`, nil)
	cases["needs login"], _ = fake("/bin/tailscale", `{"BackendState":"NeedsLogin"}`, nil)
	cases["command fails"], _ = fake("/bin/tailscale", running, errors.New("exit status 1"))
	cases["garbage"], _ = fake("/bin/tailscale", "not json", nil)
	for name, d := range cases {
		if got := d.detect(context.Background()); got.Found || got.Tailnet != "" {
			t.Errorf("%s: detect = %+v; want not found", name, got)
		}
	}
}

func TestDetect_TailnetFallbacks(t *testing.T) {
	cases := map[string]string{
		`{"BackendState":"Running","MagicDNSSuffix":"tail9.ts.net."}`:        "tail9.ts.net",
		`{"BackendState":"Running","CurrentTailnet":{"Name":"example.com"}}`: "example.com",
		`{"BackendState":"Running"}`:                                         "",
	}
	for out, want := range cases {
		d, _ := fake("/bin/tailscale", out, nil)
		got := d.detect(context.Background())
		if !got.Found || got.Tailnet != want {
			t.Errorf("detect(%s) = %+v; want found, tailnet %q", out, got, want)
		}
	}
}

func TestDetect_TimesOut(t *testing.T) {
	d, _ := fake("/bin/tailscale", running, nil)
	d.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	if got := d.detect(context.Background()); got.Found {
		t.Errorf("detect = %+v after a hang; want not found", got)
	}
	if elapsed := time.Since(start); elapsed > Timeout+time.Second {
		t.Errorf("detect took %s; want about %s", elapsed, Timeout)
	}
}
