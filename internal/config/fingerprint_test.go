package config

import "testing"

func TestFingerprint(t *testing.T) {
	on, off := true, false
	unset := Config{}
	if unset.Fingerprint() != (Config{Tailscale: Tailscale{Enabled: &on}}).Fingerprint() {
		t.Error("an unset tailscale.enabled differs from its default")
	}
	if unset.Fingerprint() == (Config{Tailscale: Tailscale{Enabled: &off}}).Fingerprint() {
		t.Error("turning Tailscale off didn't change the fingerprint")
	}
	named := Config{Gateway: Gateway{Name: "Home"}}
	if named.Fingerprint() == unset.Fingerprint() {
		t.Error("a gateway setting didn't change the fingerprint")
	}
}
