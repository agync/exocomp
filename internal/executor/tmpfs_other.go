//go:build !linux

package executor

import "errors"

// statfsProbe has no answer off Linux: this project's deployment target is a
// rootless Linux container either way, so a non-Linux dev box degrades loudly
// through the same "not usable" path tmpfsUsable already reports for any other
// probe failure, rather than pretending.
func statfsProbe(dir string) (fsType int64, freeBytes uint64, err error) {
	return 0, 0, errors.New("executor: filesystem type probe is Linux-only")
}

// tmpfsMagic never matches off Linux, so tmpfsUsable always reports false
// there (via the statfsProbe error above, which short-circuits before this
// would even be compared).
const tmpfsMagic = int64(-1)
