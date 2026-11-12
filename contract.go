// Package outpost carries the service contract, outpost-service-spec.md, in
// the binary, so a running Outpost can hand out the contract it actually
// implements rather than one that may have moved on. It lives at the root
// because go:embed can't reach above its own package's directory.
package outpost

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed outpost-service-spec.md
var serviceSpec string

// ServiceSpec is the text of outpost-service-spec.md as this build has it.
func ServiceSpec() string { return serviceSpec }

// contractRevision is worked out once: the text can't change under a running
// binary.
var contractRevision = func() string {
	sum := sha256.Sum256([]byte(serviceSpec))
	return "sha256:" + hex.EncodeToString(sum[:])
}()

// ContractRevision identifies this exact text of the contract. An app records
// it when it is built against the contract, and a different value later means
// the contract has changed and is worth reading again. It is a content hash
// rather than a number someone has to remember to bump, so it can't go stale.
func ContractRevision() string { return contractRevision }
