package shepherd

// Cross-component integration test: the supervisor's rule engine wired into
// the merge gate's reviewer (plan 04 §3, T3.4). It stays at the root because
// it exercises the scope-tree and supervision components together through
// the public surface — the components' own suites live in internal/scope and
// internal/supervision.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// withinDrafts mirrors the merge gate's drafts-prefix rule: prefix itself or
// inside prefix/.
func withinDrafts(p string) bool {
	return p == "drafts" || strings.HasPrefix(p, "drafts/")
}

// TestSupervisor_ReviewMerge covers T3.4: rule engines get merge proposals as
// events, InterventionDiscard gains a live emitter, and wiring the verdict
// into CommitMerge denies without merging.
func TestSupervisor_ReviewMerge(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("gate:rev-parent", NewLocalGitSandbox(repo))

	sup := NewSupervisor(mgr, nil) // CheckCall-only: no bus needed
	sup.AddRule(SupervisionRule{
		Name: "drafts_only_merge_gate",
		Match: func(e EffectEvent) bool {
			return e.SchemaRef == SchemaMergeProposed
		},
		Action: func(e EffectEvent) *Intervention {
			paths, _ := e.Payload["paths"].([]string)
			for _, p := range paths {
				if !withinDrafts(p) {
					return &Intervention{
						Type:    InterventionDiscard,
						ScopeID: "scope:" + e.TraceOwnerID,
						Payload: "changes must stay under drafts/",
						Time:    time.Now(),
					}
				}
			}
			return nil
		},
	})

	reviewer := func(p MergeProposal) error {
		if iv := sup.ReviewMerge(p.ChildScopeID, p); iv != nil && iv.Type == InterventionDiscard {
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
	var denied *SupervisorDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CommitMerge via ReviewMerge: err = %T (%v), want SupervisorDeniedError", err, err)
	}
	if !strings.Contains(denied.Error(), "drafts/") {
		t.Errorf("denial text = %q, want the rule's payload as the reason", denied.Error())
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	if !fileExists(t, repo, "elsewhere.txt") {
		// The shared child's write is in the parent tree; the caller's
		// checkpoint restore removes it. Here we only assert the causal side.
		t.Log("note: shared child's write was already in the parent tree; rollback is the caller's restore")
	}

	// An in-prefix proposal produces no intervention (approval).
	good := MergeProposal{
		ChildScopeID: "scope:gate:rev-child",
		Changes:      []ProposedChange{{Path: "drafts/ok.txt", Kind: ChangeCreate}},
	}
	if iv := sup.ReviewMerge(child.ID(), good); iv != nil {
		t.Errorf("in-prefix proposal: intervention = %+v, want nil", iv)
	}
}
