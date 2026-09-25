package wire_test

import (
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestNewCheckListCarriesNewKindConfig pins that a reader can see what the new
// kinds probe: the dns/tls/metric target config reaches CheckTargetView and
// survives JSON encoding.
func TestNewCheckListCarriesNewKindConfig(t *testing.T) {
	rows := []store.Check{
		{ID: 1, Name: "cert", Kind: store.CheckTLS, IntervalS: 3600, Enabled: true,
			Targets: []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443, MinDays: 21}}},
		{ID: 2, Name: "resolve", Kind: store.CheckDNS, IntervalS: 3600, Enabled: true,
			Targets: []store.CheckTarget{{Name: "web", DNSName: "web.lan", ExpectAddr: "10.0.0.5"}}},
		{ID: 3, Name: "lag", Kind: store.CheckMetric, IntervalS: 3600, Enabled: true,
			Targets: []store.CheckTarget{{Name: "q", URL: "http://web.lan/m", Op: "<", Value: 5}}},
	}
	views := wire.NewCheckList(rows, nil)
	if len(views) != 3 {
		t.Fatalf("views = %d, want 3", len(views))
	}
	byName := map[string]wire.CheckView{}
	for _, v := range views {
		byName[v.Name] = v
	}

	tls := byName["cert"].Targets[0]
	if tls.Address != "web.lan" || tls.Port != 443 || tls.MinDays != 21 {
		t.Fatalf("tls target view = %+v", tls)
	}
	dns := byName["resolve"].Targets[0]
	if dns.DNSName != "web.lan" || dns.ExpectAddr != "10.0.0.5" {
		t.Fatalf("dns target view = %+v", dns)
	}
	metric := byName["lag"].Targets[0]
	if metric.URL != "http://web.lan/m" || metric.Op != "<" || metric.Value != 5 {
		t.Fatalf("metric target view = %+v", metric)
	}

	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"min_days":21`, `"dns_name":"web.lan"`, `"expect_addr":"10.0.0.5"`, `"op"`, `"value":5`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("encoded check list missing %s: %s", want, raw)
		}
	}
}

// TestNewRuleListCarriesLimitHosts: a rule's governed --limit-hosts expansion
// reaches `rule list --output json`, and an unset flag is omitted.
func TestNewRuleListCarriesLimitHosts(t *testing.T) {
	rows := []store.Rule{{
		ID: 1, Name: "present-update", SourceFilter: "check",
		CELExpr: `event.labels["aggregate"] == "true"`, ActionKind: store.ActionPlaybook,
		Enabled: true,
		Action:  store.RuleAction{Source: "ops", Playbook: "playbooks/update.yml", LimitHosts: true},
	}}
	views := wire.NewRuleList(rows)
	if len(views) != 1 || !views[0].LimitHosts {
		t.Fatalf("rule view = %+v, want limit_hosts true", views)
	}
	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"limit_hosts":true`) {
		t.Fatalf("encoded rule list missing limit_hosts: %s", raw)
	}

	views = wire.NewRuleList([]store.Rule{{ID: 2, Name: "fixed", ActionKind: store.ActionPlaybook, Enabled: true,
		Action: store.RuleAction{Source: "ops", Playbook: "p.yml", Limit: "web"}}})
	raw, err = json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"limit_hosts"`) {
		t.Fatalf("a fixed-limit rule emitted limit_hosts: %s", raw)
	}
}

// TestNewCheckListOmitsUnsetNewFields pins the additive, backward-compatible
// shape: a tcp target emits none of the new keys, so an existing reader sees
// the same JSON it did before.
func TestNewCheckListOmitsUnsetNewFields(t *testing.T) {
	rows := []store.Check{
		{ID: 1, Name: "at-home", Kind: store.CheckTCP, IntervalS: 300, Enabled: true,
			Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}}},
	}
	raw, err := json.Marshal(wire.NewCheckList(rows, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"dns_name"`, `"expect_addr"`, `"min_days"`, `"op"`, `"value"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("tcp target emitted %s: %s", key, raw)
		}
	}
}
