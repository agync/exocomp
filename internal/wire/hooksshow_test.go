package wire_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

func TestGoldenHooksShow(t *testing.T) {
	view := wire.HooksView{
		Source:    "web",
		HooksPath: "/srv/checkout/web/.exocomp/hooks.yml",
		PreRun: []wire.HookStepView{
			{Name: "migrate", Run: "manage.py migrate"},
		},
		PostRun: []wire.HookStepView{
			{Name: "notify", Script: "scripts/notify.sh", Secret: "vault/slack"},
		},
		Gate: []wire.GateProbeView{
			{Name: "health", Kind: "http"},
		},
	}
	got, err := wire.MarshalSuccess(wire.KindHooksShow, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"hooks.show","data":{"source":"web",` +
		`"hooks_path":"/srv/checkout/web/.exocomp/hooks.yml",` +
		`"pre_run":[{"name":"migrate","run":"manage.py migrate"}],` +
		`"post_run":[{"name":"notify","script":"scripts/notify.sh","secret":"vault/slack"}],` +
		`"gate":[{"name":"health","kind":"http"}]}}` + "\n"
	if string(got) != want {
		t.Fatalf("hooks show bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenHooksShowNoHooks(t *testing.T) {
	view := wire.HooksView{
		Source:  "web",
		PreRun:  []wire.HookStepView{},
		PostRun: []wire.HookStepView{},
		Gate:    []wire.GateProbeView{},
	}
	got, err := wire.MarshalSuccess(wire.KindHooksShow, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"hooks.show","data":{"source":"web","pre_run":[],"post_run":[],"gate":[]}}` + "\n"
	if string(got) != want {
		t.Fatalf("empty hooks show bytes drift:\n got: %s\nwant: %s", got, want)
	}
}
