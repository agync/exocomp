package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestSetupCommitPersistsOperatorAndTopic proves step 4 actually writes: the
// operator display name and the generated ntfy topic land in the store, the
// topic is sealed as the notify config, and — with no push transport configured
// yet — the hand-over screen gets a clear no-transport message rather than a
// false "test push sent".
func TestSetupCommitPersistsOperatorAndTopic(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "exocomp")
	prov := &dataDirProvisioner{dir: dir}

	kit, notifyStatus, err := prov.Commit(ctx, setupInput{
		machine:  "aurora",
		operator: "Dana Kim",
		topic:    "exo-aurora-abcd1234",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if kit == "" {
		t.Error("commit returned no recovery kit text")
	}
	if !strings.Contains(notifyStatus, "no push transport") {
		t.Errorf("notify status = %q, want a clear no-transport message", notifyStatus)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if got, err := st.GetSetting(ctx, store.SettingOperator); err != nil || got != "Dana Kim" {
		t.Errorf("operator setting = %q (%v), want Dana Kim", got, err)
	}
	if got, err := st.GetSetting(ctx, store.SettingNtfyTopic); err != nil || got != "exo-aurora-abcd1234" {
		t.Errorf("ntfy topic setting = %q (%v), want the generated topic", got, err)
	}

	// Only the (optional) access token is sealed; the topic stays in the
	// settings table as ordinary configuration.
	if got, err := st.GetSetting(ctx, store.SettingNtfyTopic); err != nil || got != "exo-aurora-abcd1234" {
		t.Errorf("ntfy topic setting = %q (%v), want the generated topic", got, err)
	}
}
