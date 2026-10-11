package task

// In-package, because it exercises Abandon through the unexported
// newTaskControl and ChildHandle fields the public constructors do not
// expose.

import (
	"context"
	"testing"

	"github.com/buchenberg/shepherd-kernel-go/internal/rttest"
)

// TestAbandonWithoutTerminalCutoffFails pins Abandon's single read: a child
// frontier that does not exist is an error, not an infinite poll.
func TestAbandonWithoutTerminalCutoffFails(t *testing.T) {
	store := rttest.NewMemStore(t)
	handle := &ChildHandle{store: store, executionID: "exec:x", frontierID: "frontier:never", relationID: "rel:y"}
	control := newTaskControl(store, "exec:parent", "abandon:run", "")
	if _, err := control.Abandon(context.Background(), handle); err == nil {
		t.Fatal("Abandon of a child with no terminal cutoff succeeded, want a loud error")
	}
}
