package supervisor_test

// Cross-component integration test: the supervisor's rule engine wired into
// the merge gate's reviewer (plan 04 §3, T3.4). It lives with the supervisor
// because it exercises the rule engine and the scope tree together through
// the public surface.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/buchenberg/shepherd-kernel-go/internal/gittest"
	"github.com/buchenberg/shepherd-kernel-go/internal/rttest"
	"github.com/buchenberg/shepherd-kernel-go/sandbox/git"
	"github.com/buchenberg/shepherd-kernel-go/supervisor"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// withinDrafts mirrors the merge gate's drafts-prefix rule: prefix itself or
// inside prefix/.
func withinDrafts(p string) bool {
	return p == "drafts" || strings.HasPrefix(p, "drafts/")
}

// TestSupervisor_ReviewMerge covers T3.4: rule engines get merge proposals as
// events, supervisor.InterventionDiscard gains a live emitter, and wiring the verdict
// into CommitMerge denies without merging.

// fileExists reports whether a path exists in the test repo.
func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestSupervisor_ReviewMerge(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	parent, _ := mgr.Create("gate:rev-parent", git.NewLocalGitSandbox(repo))

	sup := supervisor.NewSupervisor(mgr, nil) // CheckCall-only: no bus needed
	sup.AddRule(supervisor.SupervisionRule{
		Name: "drafts_only_merge_gate",
		Match: func(e trace.EffectEvent) bool {
			return e.SchemaRef == shepherd.SchemaMergeProposed
		},
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			paths, _ := e.Payload["paths"].([]string)
			for _, p := range paths {
				if !withinDrafts(p) {
					return &supervisor.Intervention{
						Type:    supervisor.InterventionDiscard,
						ScopeID: "scope:" + e.TraceOwnerID,
						Payload: "changes must stay under drafts/",
						Time:    time.Now(),
					}
				}
			}
			return nil
		},
	})

	reviewer := func(p shepherd.MergeProposal) error {
		if iv := sup.ReviewMerge(p.ChildScopeID, p); iv != nil && iv.Type == supervisor.InterventionDiscard {
			return errors.New(fmt.Sprint(iv.Payload))
		}
		return nil
	}

	// A shared-sandbox child writes outside the prefix, and the supervisor's
	// rule denies the merge through ReviewMerge — the emitter story in one flow.
	child, err := mgr.Fork(parent.ID(), "gate:rev-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "elsewhere.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = mgr.CommitMerge(context.Background(), child.ID(), reviewer)
	var denied *shepherd.SupervisorDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CommitMerge via ReviewMerge: err = %T (%v), want shepherd.SupervisorDeniedError", err, err)
	}
	if !strings.Contains(denied.Error(), "drafts/") {
		t.Errorf("denial text = %q, want the rule's payload as the reason", denied.Error())
	}
	if child.State() != shepherd.ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	// A denial is causal-only: a shared-sandbox child's write is already in
	// the parent tree, so Discard removes nothing from disk — the caller's
	// checkpoint restore is the physical rollback. Assert the persistence
	// explicitly rather than logging a dead "file gone" branch.
	if !fileExists(t, repo, "elsewhere.txt") {
		t.Error("elsewhere.txt vanished: a denied shared-sandbox merge must leave the parent tree untouched")
	}

	// An in-prefix proposal produces no intervention (approval).
	good := shepherd.MergeProposal{
		ChildScopeID: "scope:gate:rev-child",
		Changes:      []shepherd.ProposedChange{{Path: "drafts/ok.txt", Kind: shepherd.ChangeCreate}},
	}
	if iv := sup.ReviewMerge(child.ID(), good); iv != nil {
		t.Errorf("in-prefix proposal: intervention = %+v, want nil", iv)
	}
}
