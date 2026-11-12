package outpost

import (
	"os"
	"strings"
	"testing"
)

// The binary carries the file as it is in the tree, not a copy that drifts.
func TestServiceSpec_IsTheFileInTheTree(t *testing.T) {
	b, err := os.ReadFile("outpost-service-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if ServiceSpec() != string(b) {
		t.Error("embedded contract differs from outpost-service-spec.md")
	}
}

func TestContractRevision_IsAContentHash(t *testing.T) {
	r := ContractRevision()
	if !strings.HasPrefix(r, "sha256:") || len(r) != len("sha256:")+64 {
		t.Errorf("revision = %q", r)
	}
}
