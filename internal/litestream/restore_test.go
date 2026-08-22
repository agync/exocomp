package litestream

import (
	"reflect"
	"testing"
)

func TestRestoreArgs(t *testing.T) {
	got, err := RestoreArgs("/data/litestream.yml", "/data/exocomp.db")
	if err != nil {
		t.Fatalf("restore args: %v", err)
	}
	want := []string{"restore", "-config", "/data/litestream.yml", "-if-replica-exists", "-o", "/data/exocomp.db", "/data/exocomp.db"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestRestoreArgsRejectsEmpty(t *testing.T) {
	if _, err := RestoreArgs("", "/data/exocomp.db"); err == nil {
		t.Error("empty config path: want error")
	}
	if _, err := RestoreArgs("/data/litestream.yml", ""); err == nil {
		t.Error("empty db path: want error")
	}
}
