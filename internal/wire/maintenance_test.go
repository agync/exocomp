package wire_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

func TestNewMaintenanceViewSafety(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cordoned bool
		inFlight int
		safe     bool
	}{
		{cordoned: false, inFlight: 0, safe: false}, // not cordoned: still taking work
		{cordoned: true, inFlight: 2, safe: false},  // cordoned but jobs in flight
		{cordoned: true, inFlight: 0, safe: true},   // cordoned and drained
		{cordoned: false, inFlight: 3, safe: false},
	}
	for _, c := range cases {
		got := wire.NewMaintenanceView(c.cordoned, c.inFlight)
		if got.SafeToReboot != c.safe {
			t.Errorf("NewMaintenanceView(%v, %d).SafeToReboot = %v, want %v",
				c.cordoned, c.inFlight, got.SafeToReboot, c.safe)
		}
		if got.Cordoned != c.cordoned || got.InFlight != c.inFlight {
			t.Errorf("view = %+v, want cordoned=%v in_flight=%d", got, c.cordoned, c.inFlight)
		}
	}
}

func TestMaintenanceViewEnvelope(t *testing.T) {
	t.Parallel()
	b, err := wire.MarshalSuccess(wire.KindMaintenance, wire.NewMaintenanceView(true, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"schema_version":1,"kind":"maintenance.readiness","data":{"cordoned":true,"in_flight":0,"safe_to_reboot":true}}` + "\n"
	if string(b) != want {
		t.Fatalf("envelope =\n%s\nwant\n%s", b, want)
	}
}
