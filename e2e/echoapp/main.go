// Command echoapp is the service the browser test puts behind the gateway. It
// reports what reached it, so the test can see from inside the app what the
// gateway passed on and what it kept back: the identity headers, and whether
// any Outpost cookie got through.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	port := flag.Int("port", 0, "TCP port to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// A page on the app's own origin that tries to frame the portal, which
	// the portal's frame-ancestors must refuse.
	mux.HandleFunc("/frame", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>frame</title><iframe id="portal" src=%q></iframe>`, r.URL.Query().Get("src"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen := map[string]any{"headers": map[string]string{}, "cookies": []string{}}
		for name, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-outpost-") {
				seen["headers"].(map[string]string)[name] = strings.Join(v, ", ")
			}
		}
		for _, c := range r.Cookies() {
			seen["cookies"] = append(seen["cookies"].([]string), c.Name)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := json.Marshal(seen) // a map of strings always encodes
		fmt.Fprintf(w, "<!doctype html><title>echo</title><pre id=\"seen\">%s</pre>", htmlEscape(string(b)))
	})
	if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
