package web

import (
	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/notify"
)

// usedByLeg maps a sealed secret's name to the /system leg that reads it
// (SPEC §7 A2 "who uses this"). The map is exact — a secret is listed only
// when a leg actually consumes it, so a rotation names the real consumer and
// never claims a consumer that does not exist. A backup target's restic
// password is never a sealed row (it is derived from the DEK as
// "restic-repo:<target>" at run time), so no backup/*/password key exists to
// annotate here.
func usedByLeg(name string) usedByRow {
	switch name {
	case notify.SecretNtfyToken:
		return usedByRow{Name: "ntfy", Href: "/system"}
	case deadman.SecretHeartbeatURL:
		return usedByRow{Name: "Dead-man", Href: "/system/deadman"}
	}
	return usedByRow{}
}
