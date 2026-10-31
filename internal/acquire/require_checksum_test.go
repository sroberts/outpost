package acquire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// An update over a verified release asks for a checksum, and without one
// nothing may land at dest.
func TestInstall_RequireChecksumRefusesAnUnverifiedAsset(t *testing.T) {
	_, c := hubWithBinary(t, []byte("binary"), false)
	c.RequireChecksum = true
	rel, _ := c.LatestRelease(context.Background(), repo)
	asset, _ := c.SelectAsset(rel, &Descriptor{})

	if err := c.CheckVerifiable(context.Background(), rel, asset.Name); !errors.As(err, new(*UnverifiedError)) {
		t.Errorf("CheckVerifiable = %v; want an *UnverifiedError", err)
	}
	dest := filepath.Join(t.TempDir(), "svc")
	_, err := c.Install(context.Background(), rel, asset, dest)
	if !errors.As(err, new(*UnverifiedError)) {
		t.Fatalf("Install = %v; want an *UnverifiedError", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a binary was installed with no checksum to verify it")
	}
}

func TestInstall_RequireChecksumAcceptsAVerifiedAsset(t *testing.T) {
	_, c := hubWithBinary(t, []byte("binary"), true)
	c.RequireChecksum = true
	rel, _ := c.LatestRelease(context.Background(), repo)
	asset, _ := c.SelectAsset(rel, &Descriptor{})

	if err := c.CheckVerifiable(context.Background(), rel, asset.Name); err != nil {
		t.Errorf("CheckVerifiable: %v", err)
	}
	res, err := c.Install(context.Background(), rel, asset, filepath.Join(t.TempDir(), "svc"))
	if err != nil || !res.Verified {
		t.Fatalf("Install = %+v, %v; want a verified install", res, err)
	}
}
