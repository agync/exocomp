package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden rewrites the fixtures from the current renderers when set
// (`go test ./internal/tui -update`). It defaults off, so an ordinary run — the
// pre-commit gate included — compares byte-for-byte and never rewrites.
var updateGolden = flag.Bool("update", false, "rewrite golden fixtures from the current output")

// readFrame returns the golden fixture's bytes as a string.
func readFrame(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "frames", name+".txt"))
	if err != nil {
		t.Fatalf("read frame %s: %v", name, err)
	}
	return string(b)
}

// assertFrame compares got against the fixture byte-for-byte, printing the
// first differing line (with a visible column ruler) so a one-space drift is
// obvious.
func assertFrame(t *testing.T, name, got string) {
	t.Helper()
	if *updateGolden {
		path := filepath.Join("testdata", "frames", name+".txt")
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update frame %s: %v", name, err)
		}
		return
	}
	want := readFrame(t, name)
	if got == want {
		return
	}
	gl := strings.Split(got, "\n")
	wl := strings.Split(want, "\n")
	n := len(gl)
	if len(wl) > n {
		n = len(wl)
	}
	t.Errorf("frame %s differs (got %d lines, want %d lines)", name, len(gl), len(wl))
	for i := 0; i < n; i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Errorf("line %d:\n got |%s| (w=%d)\nwant |%s| (w=%d)", i, g, rw(g), w, rw(w))
		}
	}
}

func home80Model() HomeModel {
	return HomeModel{
		title: "exocomp · aurora", clock: "14:03 UTC+11", tick: "tick 2s",
		needsCount: 2,
		needs: []homeNeed{
			{label: "approval", body: "core-infra plan · 1 DESTROY", right: "expires 06:41", tag: "[5]"},
			{label: "backup", body: "media-store STALE 26h (max 24h)", right: "run now", tag: "[2]→b"},
		},
		queue: "● 1 running · 2 queued · oldest 4m 12s",
		backups: []homeBackup{
			{name: "nightly-db", age: "2h", status: "ok"},
			{name: "media-store", age: "26h", status: "STALE"},
			{name: "configs", age: "6h", status: "ok"},
			{name: "restore drill", age: "12d", status: "ok · monthly"},
		},
		ws: []homeWs{
			{name: "core-infra", status: "DRIFT · 3"},
			{name: "dns-edge", status: "in sync"},
			{name: "staging", status: "in sync"},
		},
		nextLine: "02:00 nightly-db · fires in 11h 57m",
		signal:   "dead-man 12m · ntfy 2m · disk 71% · 0.7.4↗",
	}
}

func home120Model() HomeModel {
	return HomeModel{
		title: "exocomp · aurora · up 41d", clock: "14:03 UTC+11", tick: "tick 2s",
		needsCount: 2,
		needs: []homeNeed{
			{label: "approval", body: "plan #482 core-infra · 1 DESTROY · 2 change · 1 create · by nightly-plan", right: "expires 06:41 → auto-reject", tag: "[5]"},
			{label: "backup", body: "media-store last good 26h ago (max 24h · 3 attempts failed overnight)", right: "run now", tag: "[2]→b"},
		},
		queue: "1 running · 2 queued · oldest 4m 12s",
		runs: []homeRun{
			{head: "● backup nightly-db", body: "running 00:41 · step 2/4 pg_dump · 1.2 GB so far", right: "follow", tag: "[1]→enter"},
		},
		backups: []homeBackup{
			{name: "nightly-db", age: "2h", status: "ok", detail: "4.1 GB · 00:38"},
			{name: "media-store", age: "26h", status: "STALE ssh timeout"},
			{name: "configs", age: "6h", status: "ok", detail: "12 MB · 00:02"},
			{name: "media-derived", age: "--", status: "disabled"},
			{name: "restore drill", age: "12d", status: "ok", detail: "monthly"},
		},
		ws: []homeWs{
			{name: "core-infra", status: "DRIFT · 3 seen 13:12"},
			{name: "dns-edge", status: "in sync · applied 2d"},
			{name: "staging", status: "in sync · applied 6h"},
		},
		next: []homeNext{
			{at: "02:00", name: "nightly-db", when: "in 11h 57m"},
			{at: "03:00", name: "configs", when: "in 12h 57m"},
			{at: "Mon 04:00", name: "restore drill", when: "in 2d 13h"},
		},
		signal: "dead-man 12m · ntfy 2m · disk 71% · 0.7.4↗ update available",
	}
}

func TestHome80Golden(t *testing.T)  { assertFrame(t, "home-80x24", renderHome80(home80Model())) }
func TestHome120Golden(t *testing.T) { assertFrame(t, "home-120x36", renderHome120(home120Model())) }

func TestApprovalDetailGolden(t *testing.T) {
	m := ApprovalModel{
		title: "approvals · 1 waiting", clock: "14:03 UTC+11",
		plan:  "PLAN #482 · core-infra · by schedule nightly-plan",
		right: "expires 06:41 → auto-REJECT",
		destroys: []approvalItem{
			{addr: "aws_db_instance.legacy_reports", detail: "final_snapshot = false · engine pg 14.2 · 210 GB · created 2023-04"},
		},
		changes: []approvalItem{
			{addr: "aws_security_group.core", detail: "ingress 0.0.0.0/0 → 10.0.0.0/8"},
			{addr: "module.dns.record.api", detail: "ttl 300 → 60"},
		},
		creates:   []approvalItem{{addr: "aws_s3_bucket.reports-archive"}},
		secrets:   []string{"var.db_root_password = [redacted · secret]"},
		freshness: "plan age 2m · workspace unchanged since plan ✓",
	}
	assertFrame(t, "approval-detail-80x24", renderApprovalDetail(m))
}

func TestEventsGolden(t *testing.T) {
	m := EventsModel{
		summary: "events · 17 in last 24h · 11 ignored · 2 matched · 3 suppressed · 1 gated · 0 fired",
		clock:   "14:03 UTC+11",
		rows: []eventRow{
			{time: "14:02", event: "disk-pressure", source: "aurora", outcome: "SUPPRESSED", reason: "silence 'maintenance' until 15:00 · by dana", selected: true},
			{time: "13:58", event: "drift-detected", source: "core-infra", outcome: "GATED", reason: "plan #482 awaiting approval"},
			{time: "13:40", event: "backup-failed", source: "media-nas", outcome: "FIRED", reason: "ran backup-retry · succeeded 13:42"},
			{time: "13:37", event: "cert-expiry-30d", source: "edge-01", outcome: "MATCHED", reason: "queued behind running job · position 2"},
			{time: "13:12", event: "cpu-steal", source: "aurora", outcome: "IGNORED", reason: "no rule matches"},
			{time: "12:58", event: "disk-pressure", source: "aurora", outcome: "SUPPRESSED", reason: "dedup 15m · same event 12:51"},
			{time: "12:51", event: "disk-pressure", source: "aurora", outcome: "FIRED", reason: "ran disk-clean · succeeded 12:53 · freed 18 GB"},
			{time: "11:04", event: "ssh-bruteforce", source: "edge-01", outcome: "IGNORED", reason: "no rule matches"},
		},
	}
	assertFrame(t, "events-120", renderEventsScreen(m))
}

func TestSourcesGolden(t *testing.T) {
	m := SourcesModel{
		summary: "sources · 4 registered", clock: "14:03 UTC+11",
		rows: []sourceRow{
			{name: "core-infra", kind: "git", ref: "refs/heads/main", sync: "a1b2c3d4e5f6", trust: "SIGNED · SIGNERS", enabled: "yes"},
			{name: "playbooks", kind: "git", ref: "refs/tags/v3", sync: "9f8e7d6c5b4a", trust: "SIGNED", enabled: "yes"},
			{name: "host-media", kind: "hostpath", ref: "-", sync: "-", trust: "unsigned", enabled: "yes"},
			{name: "vol-scratch", kind: "volpath", ref: "-", sync: "-", trust: "unsigned", enabled: "no"},
		},
	}
	assertFrame(t, "sources-120", renderSourcesScreen(m))
}

func TestTraceGolden(t *testing.T) {
	m := TraceModel{
		title: "trace · disk-pressure @ 14:02:11",
		steps: []traceStep{
			{n: 1, text: "rule 'disk-pressure' pattern host=aurora vol=/data", result: "MATCH"},
			{n: 2, text: "reactive pause", result: "off"},
			{n: 3, text: "dedup window 15m · last same event 12:51 — outside", result: "pass"},
			{n: 4, text: "silence 'maintenance' until 15:00 · set by dana 13:55", result: "HIT",
				note: "→ outcome SUPPRESSED · nothing ran · counted on the banner"},
			{n: 5, text: "approval gate", result: "not reached"},
			{n: 6, text: "notify", result: "not reached"},
		},
	}
	assertFrame(t, "trace-80", renderTrace(m))
}

func TestBannersGolden(t *testing.T) {
	lines := []string{
		bannerLine(BannerState{Paused: true, Swallowed: 3}),
		bannerLine(BannerState{Muted: true, MutedUntil: "15:00", MutedRemaining: "43m"}),
		bannerLine(BannerState{Paused: true, Muted: true, MutedUntil: "15:00", Swallowed: 3}),
		bannerLine(BannerState{Silenced: true, Swallowed: 3}),
		bannerLine(BannerState{Paused: true, Swallowed: 3, NoColor: true}),
	}
	assertFrame(t, "banners-80", join(lines))
}

func TestMuteModalGolden(t *testing.T) {
	m := muteModal{
		expiry: "for:  15m   1h   4h  until 09:00",
		reason: "reason (logged): maintenance window▍",
		until:  "15:00",
	}
	assertFrame(t, "mute-modal-60", renderMuteModal(m))
}

func TestStalePlanGolden(t *testing.T) {
	assertFrame(t, "stale-plan-refusal-80", renderStalePlan(StaleModel{workspace: "core-infra", driftSeen: "14:12"}))
}

func TestDestroyConfirmGolden(t *testing.T) {
	d := DestroyConfirm{
		planID: "482", destroys: 1,
		resource:  "aws_db_instance.legacy_reports · 210 GB · no snapshot",
		typed:     "core-inf",
		expiresAt: "06:12",
	}
	assertFrame(t, "destroy-confirm-80", renderDestroyConfirm(d))
}

func TestEmptyWorkspacesGolden(t *testing.T) {
	assertFrame(t, "empty-workspaces-80x24", renderEmptyWorkspaces())
}

func TestWizardIdentityGolden(t *testing.T) {
	w := WizardIdentity{machine: "aurora", operator: "Dana Kim", topic: "exo-aurora-8f2", cursor: true}
	assertFrame(t, "wizard-1-identity-80x24", renderWizardIdentity(w))
}

func TestWizardKitGolden(t *testing.T) {
	w := WizardKit{
		machine: "aurora",
		ageKey:  "AGE-SECRET-KEY-1QL38U…9VK7XW",
		keyID:   "aurora-2026-08",
		created: "2026-08-22 14:03 UTC+11",
		typed:   "I HAVE STO",
	}
	assertFrame(t, "wizard-2-kit-80x24", renderWizardKit(w))
}
