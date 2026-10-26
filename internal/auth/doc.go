// Package auth owns everything Outpost knows about people: users, groups,
// password hashes, sessions and the keys that sign identity tokens. It knows
// nothing about HTTP; the gateway builds on it.
//
// It keeps its own database, auth.db, rather than sharing state.db. state.db
// is a cache of observed reality that `outpost apply` can rebuild from the
// manifests, and there is a test holding it to that. Users and password
// hashes are not observed reality: nothing can rebuild them, so they live
// somewhere that can be backed up and is never treated as disposable.
package auth
