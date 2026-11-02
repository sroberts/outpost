package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Backups are filed under the name the manifest declares, so that is where
// --purge has to look when the file is called something else.
func TestRemove_PurgeFindsBackupsUnderTheDeclaredName(t *testing.T) {
	home := newHome(t)
	storage := filepath.Join(home, "data", "declared")
	body := fmt.Sprintf("name = %q\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nport = %d\nstorage = %q\nhealthcheck = \"/up\"\n",
		"declared", freePort(t), storage)
	if err := os.WriteFile(filepath.Join(home, "services", "file.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(home, "backups", "declared")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runCLI(t, home, "remove", "file", "--purge", "--yes"); err != nil {
		t.Fatalf("remove --purge: %v", err)
	}
	if _, err := os.Stat(backups); !os.IsNotExist(err) {
		t.Errorf("--purge left the service's backups behind (stat: %v)", err)
	}
}

// Once the manifest has gone, nothing knows where the data is, so remove
// mustn't suggest a command that can only fail.
func TestRemove_DoesNotSuggestAPurgeThatCanNotWork(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, home, "remove", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "outpost remove demo --purge") {
		t.Errorf("remove suggests a command that no longer works:\n%s", out)
	}
	if _, _, err := runCLI(t, home, "remove", "demo", "--purge", "--yes"); err == nil {
		t.Error("remove --purge after remove: want an error, which is why it isn't suggested")
	}
}
