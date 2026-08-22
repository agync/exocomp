//go:build !linux

package executor

import "errors"

// landlockABI has no answer off Linux: nono uses Seatbelt on macOS, and this
// project's deployment target is a rootless Linux container either way. The
// probe degrades loudly rather than pretending.
func landlockABI() (int, error) {
	return 0, errors.New("Landlock is a Linux kernel feature")
}

// LandlockNetABI is the ABI at which Landlock can filter TCP connect by port.
const LandlockNetABI = 4
