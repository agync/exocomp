package wire

import (
	"bytes"
	"encoding/json"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

func TestCommandResultView_WriteSuccess(t *testing.T) {
	hosts := map[string]executor.HostCommandResult{
		"hostA": {
			Status:  "CHANGED",
			RC:      0,
			Changed: true,
			Stdout:  "hello world",
		},
	}
	view := NewCommandResultView(368, "succeeded", 0, hosts)

	var buf bytes.Buffer
	if err := WriteSuccess(&buf, KindCommandResult, view); err != nil {
		t.Fatalf("WriteSuccess: %v", err)
	}

	var env struct {
		SchemaVersion int               `json:"schema_version"`
		Kind          string            `json:"kind"`
		Data          CommandResultView `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Kind != KindCommandResult {
		t.Errorf("kind = %q, want %q", env.Kind, KindCommandResult)
	}
	if env.Data.JobID != 368 || env.Data.State != "succeeded" || env.Data.ExitCode != 0 {
		t.Errorf("data mismatch: %+v", env.Data)
	}
	h, ok := env.Data.Hosts["hostA"]
	if !ok || h.Stdout != "hello world" || !h.Changed {
		t.Errorf("hostA mismatch: %+v", h)
	}
}
