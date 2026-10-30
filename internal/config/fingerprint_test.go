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
	gw := Gateway{Enabled: true, PublicURL: "https://outpost.example.com"}
	named := gw
	named.Name = "Home"
	if (Config{Gateway: named}).Fingerprint() == (Config{Gateway: gw}).Fingerprint() {
		t.Error("a gateway setting didn't change the fingerprint")
	}
	gw.Name = "Outpost"
	if (Config{Gateway: gw}).Fingerprint() != (Config{Gateway: Gateway{Enabled: true, PublicURL: gw.PublicURL}}).Fingerprint() {
		t.Error("writing out the default name changed the fingerprint")
	}
	if (Config{Gateway: Gateway{Name: "Home"}}).Fingerprint() != unset.Fingerprint() {
		t.Error("a setting of a gateway that's off changed the fingerprint")
	}
}
