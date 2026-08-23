//go:build !linux

package executor

import "errors"

// runDirFSType has no answer off Linux: this project's deployment target is a
// rootless Linux container, and without statfs the tmpfs assertion cannot be
// made — so it errors, and the file-staging path fails closed rather than
// staging a bearer onto a filesystem it could not prove is RAM-only.
func runDirFSType(dir string) (int64, error) {
	return 0, errors.New("executor: filesystem type probe is Linux-only")
}

// tmpfsMagic never matches off Linux, so RunDirTmpfs reports false there.
const tmpfsMagic = int64(-1)
