package config

import (
	"strings"
	"testing"
)

func TestGateway_Name(t *testing.T) {
	g := Gateway{Enabled: true, PublicURL: "https://outpost.example"}
	if g.PortalName() != "Outpost" {
		t.Errorf("default name %q", g.PortalName())
	}
	g.Name = "Home\nOutpost"
	if g.validate() == nil {
		t.Error("a two-line name was accepted")
	}
	g.Name = "Home Outpost"
	if err := g.validate(); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	g.render(&b)
	if !strings.Contains(b.String(), `name = "Home Outpost"`) {
		t.Errorf("name not written back:\n%s", b.String())
	}
}
