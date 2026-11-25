package executor_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// A materialized inventory's hosts are the chain's targets: what to wait for,
// scan, onboard, and converge. They are read from nested groups the way ansible
// reads them, with the connection facts each host declares.
func TestInventoryHostsWalksGroupsAndChildren(t *testing.T) {
	body := []byte(`{
	  "all": {
	    "hosts": {"edge": {"ansible_host": "10.0.0.9", "ansible_port": 2222}},
	    "children": {
	      "web": {"hosts": {"web1": {"ansible_host": "10.0.0.1"}, "web2": null}},
	      "control": {"children": {"inner": {"hosts": {"ctl": {"ansible_connection": "local"}}}}}
	    }
	  }
	}`)

	hosts, err := executor.InventoryHosts(body)
	if err != nil {
		t.Fatalf("InventoryHosts: %v", err)
	}
	got := map[string]executor.InventoryHost{}
	for _, h := range hosts {
		got[h.Name] = h
	}
	if len(got) != 4 {
		t.Fatalf("hosts = %v, want 4", hosts)
	}

	if h := got["edge"]; h.Address != "10.0.0.9" || h.Port != 2222 {
		t.Errorf("edge = %+v, want 10.0.0.9:2222", h)
	}
	// A host that declares no ansible_host is reached by its own name, on 22.
	if h := got["web2"]; h.Address != "web2" || h.Port != executor.SSHPort {
		t.Errorf("web2 = %+v, want web2:%d", h, executor.SSHPort)
	}
	// A local host is reached without SSH: no wait, no key scan.
	if h := got["ctl"]; !h.Local {
		t.Errorf("ctl = %+v, want Local", h)
	}
	if h := got["web1"]; h.Local {
		t.Errorf("web1 = %+v, want not Local", h)
	}
}

// The fallback shape a capture writes when the workspace declared no
// ansible_inventory output names no hosts. That is the fail-closed signal every
// caller keys on, so it must be an empty list and not an error.
func TestInventoryHostsVarFormNamesNoHosts(t *testing.T) {
	hosts, err := executor.InventoryHosts([]byte(`{"all": {"vars": {"region": "eu"}}}`))
	if err != nil {
		t.Fatalf("InventoryHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hosts)
	}
}

func TestInventoryHostsRejectsNonInventory(t *testing.T) {
	if _, err := executor.InventoryHosts([]byte("not json")); err == nil {
		t.Fatal("InventoryHosts accepted a non-inventory body")
	}
}

// TestInventoryHostsParsesAnsibleYAML proves Ansible YAML standing inventories
// parse identically to materialized JSON inventories so play schedules work
// against both formats.
func TestInventoryHostsParsesAnsibleYAML(t *testing.T) {
	body := []byte(`---
all:
  children:
    nodes:
      hosts:
        alpha:
          ansible_host: 10.0.0.10
          ansible_port: 2222
        beta:
    nested:
      children:
        local_grp:
          hosts:
            ctl:
              ansible_connection: local
`)
	hosts, err := executor.InventoryHosts(body)
	if err != nil {
		t.Fatalf("InventoryHosts YAML: %v", err)
	}
	got := map[string]executor.InventoryHost{}
	for _, h := range hosts {
		got[h.Name] = h
	}
	if len(got) != 3 {
		t.Fatalf("hosts = %v, want 3", hosts)
	}
	if h := got["alpha"]; h.Address != "10.0.0.10" || h.Port != 2222 || h.Local {
		t.Errorf("alpha = %+v, want 10.0.0.10:2222 remote", h)
	}
	if h := got["beta"]; h.Address != "beta" || h.Port != executor.SSHPort {
		t.Errorf("beta = %+v, want beta:%d", h, executor.SSHPort)
	}
	if h := got["ctl"]; !h.Local {
		t.Errorf("ctl = %+v, want Local", h)
	}
}

func TestInventoryHostsYAMLVarOnlyNamesNoHosts(t *testing.T) {
	body := []byte("all:\n  vars:\n    region: eu\n")
	hosts, err := executor.InventoryHosts(body)
	if err != nil {
		t.Fatalf("InventoryHosts YAML: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hosts)
	}
}

func TestInventoryHostsEdgeCases(t *testing.T) {
	body := []byte(`
# Commented YAML inventory
all:
  hosts:
    strport:
      ansible_host: 10.0.0.20
      ansible_port: "2222"
    overflow:
      ansible_port: 18446744073709551615
`)
	hosts, err := executor.InventoryHosts(body)
	if err != nil {
		t.Fatalf("InventoryHosts commented YAML: %v", err)
	}
	got := map[string]executor.InventoryHost{}
	for _, h := range hosts {
		got[h.Name] = h
	}
	if got["strport"].Port != 2222 {
		t.Errorf("strport = %+v, want port 2222", got["strport"])
	}
	if got["overflow"].Port != executor.SSHPort {
		t.Errorf("overflow = %+v, want fallback %d", got["overflow"], executor.SSHPort)
	}

	for _, bad := range [][]byte{
		[]byte(""),
		[]byte("null"),
		[]byte("123"),
		[]byte("- item1\n- item2\n"),
	} {
		if _, err := executor.InventoryHosts(bad); err == nil {
			t.Errorf("InventoryHosts accepted non-mapping %q", bad)
		}
	}
}
