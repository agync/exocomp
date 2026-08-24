package main

import (
	"strings"
	"testing"
)

// TestWorkspaceAddPromisesNothingItCannotDo: --auto-apply named a behaviour
// that does not exist — nothing reads the column, and scheduled drift is not yet
// honoured. A flag an operator can set and nothing honours is worse than its absence.
func TestWorkspaceAddPromisesNothingItCannotDo(t *testing.T) {
	add, _, err := newWorkspaceCmd().Find([]string{"add"})
	if err != nil {
		t.Fatalf("find workspace add: %v", err)
	}
	if flag := add.Flags().Lookup("auto-apply"); flag != nil {
		t.Error("workspace add still offers --auto-apply, which nothing implements")
	}
	if strings.Contains(add.Long+add.Short, "auto-apply") {
		t.Error("workspace add still advertises auto-apply in its help")
	}
}
