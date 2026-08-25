package main

import (
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// provisionedTofuOutput is the `tofu output -json` a stand-in tofu prints for
// the provision chain: one host reached over SSH on a non-default port, which
// is what the wait step probes and the key scan records.
const provisionedTofuOutput = `{"ansible_inventory":{"sensitive":false,"value":` +
	`{"all":{"children":{"web":{"hosts":{"node1":` +
	`{"ansible_host":"198.51.100.7","ansible_port":2222,"ansible_user":"admin"}}}}}}}}`

// fakeKeyscan is an ssh-keyscan that answers for whatever host and port it is
// given, in the `[host]:port` form known_hosts uses off port 22.
const fakeKeyscan = `host=""
port=22
while [ $# -gt 0 ]; do
  case "$1" in
    -p) port="$2"; shift 2 ;;
    -T|-t) shift 2 ;;
    *) host="$1"; shift ;;
  esac
done
if [ "$port" = "22" ]; then name="$host"; else name="[$host]:$port"; fi
echo "$name ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPROVISIONFAKE"
exit 0`

// TestProvisionChainOnboardsAndConverges is items A of the chain end to end: an
// apply materializes an inventory naming a host nobody has ever reached, and
// the three ansible steps that follow — wait, onboard, converge — all target
// that inventory, in order, as one ledger workflow.
func TestProvisionChainOnboardsAndConverges(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.TofuBinary,
		"case \"$1\" in output) printf '%s' '"+provisionedTofuOutput+"' ;; esac\nexit 0")
	fakeTool(t, "ssh-keyscan", fakeKeyscan)
	// Stand-in ansible-playbook: report its argv and the inventory it was
	// handed, so each step's job events say what it actually ran against.
	fakeTool(t, executor.AnsibleBinary, `inv=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--inventory" ]; then inv="$a"; fi
  prev="$a"
done
echo "PLAY_ARGV=$*"
if [ -n "$inv" ]; then echo "PLAY_INV_BEGIN"; cat "$inv"; echo "PLAY_INV_END"; fi
exit 0`)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "edge", "--dir", module)
	playbook := module + "/converge.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "disabled")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "provision", "run", "edge", playbook, "--wait")
	if code != 0 {
		t.Fatalf("provision run exited %d:\n%s\n%s", code, out, d.dump())
	}

	workflowID := firstMatch(t, out, `workflow (provision:\S+) started`)
	steps := workflowSteps(t, workflowID)
	wantKinds := []string{"tofu-apply", "ssh-wait", "host-onboard", "ansible-play"}
	if len(steps) != len(wantKinds) {
		t.Fatalf("workflow has %d steps, want %d:\n%s", len(steps), len(wantKinds), d.dump())
	}
	for i, want := range wantKinds {
		if steps[i].Kind != want {
			t.Errorf("step %d is %q, want %q", i+1, steps[i].Kind, want)
		}
		if steps[i].Status != store.StepSucceeded {
			t.Errorf("step %d (%s) is %s, want succeeded\n%s", i+1, steps[i].Kind, steps[i].Status, d.dump())
		}
	}

	// The wait step ran the seeded reachability runbook with the bound timeout,
	// against the inventory the apply had just materialized.
	waitEvents := jobEventText(t, *steps[1].JobID)
	if !strings.Contains(waitEvents, "wait-ssh.yml") {
		t.Errorf("the wait step did not run the seeded runbook:\n%s", waitEvents)
	}
	if !strings.Contains(waitEvents, "exocomp_ssh_wait_timeout") {
		t.Errorf("the wait step carried no bounded timeout:\n%s", waitEvents)
	}
	if !strings.Contains(waitEvents, "198.51.100.7") {
		t.Errorf("the wait step did not read the materialized inventory:\n%s", waitEvents)
	}

	// The onboarding step ran the seeded onboarding runbook carrying the fleet
	// PUBLIC key, as JSON — ansible's k=v form would split an authorized_keys
	// line on its spaces.
	onboardEvents := jobEventText(t, *steps[2].JobID)
	if !strings.Contains(onboardEvents, "onboard.yml") {
		t.Errorf("the onboard step did not run the seeded runbook:\n%s", onboardEvents)
	}
	if !strings.Contains(onboardEvents, `{"exocomp_fleet_pubkey":"ssh-ed25519 `) {
		t.Errorf("the onboard step did not carry the fleet public key as JSON:\n%s", onboardEvents)
	}
	if !strings.Contains(onboardEvents, "pinned 1 host key(s) for node1 before onboarding it") {
		t.Errorf("the onboard step did not pin the host key first:\n%s", onboardEvents)
	}

	// The pin is durable: the host key was recorded under the `[host]:port`
	// name, from the port the inventory declared.
	assertKnownHostRecorded(t, "[198.51.100.7]:2222")

	// The configure play ran against the same materialized inventory.
	playEvents := jobEventText(t, *steps[3].JobID)
	if !strings.Contains(playEvents, "converge.yml") || !strings.Contains(playEvents, "node1") {
		t.Errorf("the configure play did not run against the tf-derived inventory:\n%s", playEvents)
	}

	// Item B: the standing inventory the chain refreshed is a first-class
	// target a schedule can name, and it holds the provisioned host.
	assertStandingInventoryHasHost(t, "tf:edge", "node1")
}

// A chain whose apply fails must not reach a host at all: the three ansible
// steps stay blocked, and nothing is onboarded.
func TestProvisionChainBlocksEverythingAfterAFailedApply(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.TofuBinary, `case "$1" in apply) exit 1 ;; esac
exit 0`)
	fakeTool(t, "ssh-keyscan", fakeKeyscan)
	fakeTool(t, executor.AnsibleBinary, "exit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "edge", "--dir", module)
	playbook := module + "/converge.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "disabled")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, _ := runCLIExit(t, bin, "provision", "run", "edge", playbook, "--wait")
	workflowID := firstMatch(t, out, `workflow (provision:\S+) started`)
	steps := workflowSteps(t, workflowID)
	if len(steps) != 4 {
		t.Fatalf("workflow has %d steps, want 4", len(steps))
	}
	if steps[0].Status != store.StepFailed {
		t.Errorf("apply step is %s, want failed\n%s", steps[0].Status, d.dump())
	}
	for _, s := range steps[1:] {
		if s.Status != store.StepBlocked {
			t.Errorf("step %d (%s) is %s, want blocked", s.StepNo, s.Kind, s.Status)
		}
	}
}

func assertKnownHostRecorded(t *testing.T, host string) {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	hosts, err := st.ListKnownHosts(t.Context())
	if err != nil {
		t.Fatalf("list known hosts: %v", err)
	}
	for _, kh := range hosts {
		if kh.Host == host {
			return
		}
	}
	t.Errorf("no known_host row for %s; recorded: %+v", host, hosts)
}

func assertStandingInventoryHasHost(t *testing.T, name, host string) {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	standing, err := st.StandingInventory(t.Context(), name)
	if err != nil {
		t.Fatalf("standing inventory %s: %v", name, err)
	}
	hosts, err := executor.InventoryHosts(standing.Body)
	if err != nil {
		t.Fatalf("parse standing inventory: %v", err)
	}
	for _, h := range hosts {
		if h.Name == host {
			return
		}
	}
	t.Errorf("standing inventory %s does not name %s: %+v", name, host, hosts)
}
