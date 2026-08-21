// Package backup assembles one staging tree and fans it out to every enabled
// target, then restores a data dir from any one of them.
//
// The chain that makes recovery work is documented in
// docs/operations/disaster-recovery.md: keyring.age lands at every target root,
// outside the restic repository, because the repository password is derived
// from the key that keyring wraps.
package backup
