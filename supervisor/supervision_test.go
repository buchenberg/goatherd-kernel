package supervisor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/buchenberg/shepherd-kernel-go/internal/rttest"
	"github.com/buchenberg/shepherd-kernel-go/supervisor"
	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

func TestSupervisor_BasicRuleMatch(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, bus)

	matched := false
	sup.AddRule(supervisor.SupervisionRule{
		Name: "test_rule",
		Match: func(e trace.EffectEvent) bool {
			return e.KindLabel == "bash"
		},
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			matched = true
			return &supervisor.Intervention{
				Type:    supervisor.InterventionInject,
				ScopeID: "scope:" + e.TraceOwnerID,
				Payload: "test intervention",
				Time:    time.Now(),
			}
		},
	})

	sup.Start("test-supervisor")
	defer sup.Close()

	// Publish a matching event
	bus.Publish(trace.EffectEvent{
		RecordID:     "sha256:test",
		TraceOwnerID: "sub:worker",
		Mode:         trace.Declaration,
		KindLabel:    "bash",
		Payload:      map[string]any{"cmd": "ls"},
	})

	// Wait for intervention
	select {
	case iv := <-sup.Interventions():
		if iv.Type != supervisor.InterventionInject {
			t.Errorf("expected inject, got %s", iv.Type)
		}
		if iv.ScopeID != "scope:sub:worker" {
			t.Errorf("expected scope:sub:worker, got %s", iv.ScopeID)
		}
		if !matched {
			t.Error("rule action should have been called")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for intervention")
	}
}

func TestSupervisor_NonMatchingRule(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, bus)

	sup.AddRule(supervisor.SupervisionRule{
		Name: "bash_only",
		Match: func(e trace.EffectEvent) bool {
			return e.KindLabel == "bash"
		},
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return &supervisor.Intervention{Type: supervisor.InterventionInject, ScopeID: "x"}
		},
	})

	sup.Start("test-nonmatch")
	defer sup.Close()

	// Publish a non-matching event
	bus.Publish(trace.EffectEvent{
		KindLabel: "read_file",
		Mode:      trace.Declaration,
	})

	// Should NOT receive intervention
	select {
	case <-sup.Interventions():
		t.Error("should not receive intervention for non-matching event")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestSupervisor_FirstRuleWins(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, bus)

	var firstCalled bool
	sup.AddRule(supervisor.SupervisionRule{
		Name:  "first",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			firstCalled = true
			return &supervisor.Intervention{Type: supervisor.InterventionInject, ScopeID: "x"}
		},
	})
	sup.AddRule(supervisor.SupervisionRule{
		Name:  "second",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return &supervisor.Intervention{Type: supervisor.InterventionHalt, ScopeID: "x"}
		},
	})

	sup.Start("first-wins")
	defer sup.Close()

	bus.Publish(trace.EffectEvent{KindLabel: "test"})

	select {
	case iv := <-sup.Interventions():
		if !firstCalled {
			t.Error("first rule Action should have been called")
		}
		if iv.Type != supervisor.InterventionInject {
			t.Errorf("expected supervisor.InterventionInject from first rule, got %s", iv.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for intervention")
	}
}

func TestSupervisor_ObserveOnlyRule(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, bus)

	// Rule that matches but returns nil (observe-only)
	sup.AddRule(supervisor.SupervisionRule{
		Name:  "observer",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return nil // observe only
		},
	})

	sup.Start("observer-test")
	defer sup.Close()

	bus.Publish(trace.EffectEvent{KindLabel: "test"})

	select {
	case <-sup.Interventions():
		t.Error("observe-only rule should not produce intervention")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestSupervisor_Inject(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	mgr.Create("sub:inject-target", nil)

	sup := supervisor.NewSupervisor(mgr, bus)
	err := sup.Inject("scope:sub:inject-target", "try a different approach")
	if err != nil {
		t.Fatalf("inject failed: %v", err)
	}

	// Should be recorded in the trace
	slice, err := store.ReadOwnerPrefix(context.Background(), trace.TrustedReadContext, "sub:inject-target", 99, trace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == shepherd.SchemaSupervisorInject {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected supervisor:inject record in trace")
	}
}

func TestSupervisor_Halt(t *testing.T) {
	store := rttest.NewMemStore(t)
	bus := trace.NewEffectBus(64)
	defer bus.Close()

	mgr := shepherd.NewScopeManager(store)
	target, _ := mgr.Create("sub:halt-target", nil)

	sup := supervisor.NewSupervisor(mgr, bus)
	err := sup.Halt("scope:sub:halt-target")
	if err != nil {
		t.Fatalf("halt failed: %v", err)
	}

	// the target scope should be discarded
	if target.State() != shepherd.ScopeDiscarded {
		t.Errorf("expected scope discarded after halt, got %s", target.State())
	}

	// Should be recorded in the trace
	slice, err := store.ReadOwnerPrefix(context.Background(), trace.TrustedReadContext, "sub:halt-target", 99, trace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == shepherd.SchemaSupervisorHalt {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected supervisor:halt record in trace")
	}
}

func TestSupervisor_ScopeNotFound(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, nil)

	err := sup.Inject("scope:nonexistent", "hello")
	if err == nil {
		t.Error("inject to nonexistent scope should fail")
	}

	err = sup.Halt("scope:nonexistent")
	if err == nil {
		t.Error("halt of nonexistent scope should fail")
	}
}

// --- Built-in rule tests ---

func TestDestructiveToolRule_BashRm(t *testing.T) {
	rule := supervisor.DestructiveToolRule()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "rm -rf /tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("rm command should match destructive rule")
	}

	iv := rule.Action(event)
	if iv == nil {
		t.Fatal("destructive rule should produce intervention")
	}
	if iv.Type != supervisor.InterventionInject {
		t.Errorf("expected inject, got %s", iv.Type)
	}
}

func TestDestructiveToolRule_BashSafe(t *testing.T) {
	rule := supervisor.DestructiveToolRule()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "ls -la"},
	}

	if rule.Match(event) {
		t.Error("ls command should NOT match destructive rule")
	}
}

func TestDestructiveToolRule_Write(t *testing.T) {
	rule := supervisor.DestructiveToolRule()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "write",
		SchemaRef: "yaah.tool.write.v1",
		Payload:   map[string]any{"path": "/tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("write should match destructive rule")
	}
}

func TestDestructiveToolRule_IgnoresCaptures(t *testing.T) {
	rule := supervisor.DestructiveToolRule()

	event := trace.EffectEvent{
		Mode:      trace.Capture,
		KindLabel: "bash",
		Payload:   map[string]any{"cmd": "rm -rf /"},
	}

	if rule.Match(event) {
		t.Error("capture events should not match (only intercept intents)")
	}
}

func TestHighErrorRateRule_Triggers(t *testing.T) {
	rule := supervisor.HighErrorRateRule(0.5, 6) // 50% threshold, window of 6

	// First: add 2 errors — not enough data yet (need windows/2 = 3)
	for i := 0; i < 2; i++ {
		event := trace.EffectEvent{
			TraceOwnerID: "sub:error-prone",
			Mode:         trace.Capture,
			KindLabel:    "bash:result",
			Payload:      map[string]any{"success": false},
		}
		rule.Match(event)
		iv := rule.Action(event)
		if iv != nil {
			t.Errorf("should not trigger with only %d errors", i+1)
		}
	}

	// 3rd error — triggers (3/3 = 100% >= 50%)
	event := trace.EffectEvent{
		TraceOwnerID: "sub:error-prone",
		Mode:         trace.Capture,
		KindLabel:    "bash:result",
		Payload:      map[string]any{"success": false},
	}
	rule.Match(event)
	iv := rule.Action(event)
	if iv == nil {
		t.Error("should trigger when error rate exceeds threshold")
	}

	// After trigger, results are reset. Next single error should not trigger.
	event2 := trace.EffectEvent{
		TraceOwnerID: "sub:error-prone",
		Mode:         trace.Capture,
		KindLabel:    "bash:result",
		Payload:      map[string]any{"success": false},
	}
	rule.Match(event2)
	if iv = rule.Action(event2); iv != nil {
		t.Error("should not trigger again after reset (only 1 error in buffer)")
	}
}

func TestHighErrorRateRule_IgnoresDeclarations(t *testing.T) {
	rule := supervisor.HighErrorRateRule(0.5, 6)

	event := trace.EffectEvent{
		TraceOwnerID: "sub:test",
		Mode:         trace.Declaration,
		KindLabel:    "bash",
	}

	if rule.Match(event) {
		t.Error("declarations should not match error rate rule")
	}
}

func TestStuckDetectionRule_Triggers(t *testing.T) {
	rule := supervisor.StuckDetectionRule(3) // 3 repeats

	// Same call 3 times
	for i := 0; i < 3; i++ {
		event := trace.EffectEvent{
			TraceOwnerID: "sub:stuck",
			Mode:         trace.Declaration,
			KindLabel:    "bash",
			Payload:      map[string]any{"cmd": "ls -la"},
		}
		if !rule.Match(event) {
			t.Fatalf("should match on iteration %d", i)
		}
		iv := rule.Action(event)
		if i < 2 && iv != nil {
			t.Errorf("should not trigger on repeat %d", i+1)
		}
		if i == 2 && iv == nil {
			t.Error("should trigger on repeat 3")
		}
	}
}

func TestStuckDetectionRule_ResetsOnDifferentCall(t *testing.T) {
	rule := supervisor.StuckDetectionRule(3)

	events := []trace.EffectEvent{
		{TraceOwnerID: "sub:reset", Mode: trace.Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: trace.Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: trace.Declaration, KindLabel: "read_file", Payload: map[string]any{"path": "x"}}, // different
		{TraceOwnerID: "sub:reset", Mode: trace.Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: trace.Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
	}

	for i, event := range events {
		rule.Match(event)
		iv := rule.Action(event)
		if iv != nil {
			t.Errorf("should not trigger on event %d (reset after different call)", i)
		}
	}
}

func TestStuckDetectionRule_IgnoresCaptures(t *testing.T) {
	rule := supervisor.StuckDetectionRule(3)

	event := trace.EffectEvent{
		TraceOwnerID: "sub:test",
		Mode:         trace.Capture,
		KindLabel:    "bash",
	}

	if rule.Match(event) {
		t.Error("captures should not match stuck detection")
	}
}

// --- Phase 2: CheckCall tests ---

func TestSupervisor_CheckCall_Approved(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, nil)

	// Non-matching rule
	sup.AddRule(supervisor.SupervisionRule{
		Name: "bash_only",
		Match: func(e trace.EffectEvent) bool {
			return e.KindLabel == "bash"
		},
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return &supervisor.Intervention{Type: supervisor.InterventionDeny, ScopeID: "x"}
		},
	})

	// Event that doesn't match
	iv := sup.CheckCall(trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "read_file",
	})
	if iv != nil {
		t.Errorf("expected nil (approved), got %v", iv)
	}
}

func TestSupervisor_CheckCall_Deny(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, nil)

	sup.AddRule(supervisor.DestructiveToolGuard())

	iv := sup.CheckCall(trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "rm -rf /tmp"},
	})
	if iv == nil {
		t.Fatal("expected intervention, got nil")
	}
	if iv.Type != supervisor.InterventionDeny {
		t.Errorf("expected deny, got %s", iv.Type)
	}
}

func TestSupervisor_CheckCall_FirstRuleWins(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, nil)

	sup.AddRule(supervisor.SupervisionRule{
		Name:  "first",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return &supervisor.Intervention{Type: supervisor.InterventionInject, ScopeID: "x"}
		},
	})
	sup.AddRule(supervisor.SupervisionRule{
		Name:  "second",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return &supervisor.Intervention{Type: supervisor.InterventionHalt, ScopeID: "x"}
		},
	})

	iv := sup.CheckCall(trace.EffectEvent{Mode: trace.Declaration, KindLabel: "test"})
	if iv == nil {
		t.Fatal("expected intervention")
	}
	if iv.Type != supervisor.InterventionInject {
		t.Errorf("expected inject from first rule, got %s", iv.Type)
	}
}

func TestSupervisor_CheckCall_ObserveOnly(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	sup := supervisor.NewSupervisor(mgr, nil)

	sup.AddRule(supervisor.SupervisionRule{
		Name:  "observer",
		Match: func(e trace.EffectEvent) bool { return true },
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			return nil // observe only
		},
	})

	iv := sup.CheckCall(trace.EffectEvent{Mode: trace.Declaration})
	if iv != nil {
		t.Error("observe-only rule should return nil")
	}
}

func TestDestructiveToolGuard_BashRm(t *testing.T) {
	rule := supervisor.DestructiveToolGuard()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "rm -rf /tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("rm command should match guard")
	}

	iv := rule.Action(event)
	if iv == nil {
		t.Fatal("guard should produce intervention")
	}
	if iv.Type != supervisor.InterventionDeny {
		t.Errorf("expected deny, got %s", iv.Type)
	}
}

// TestDestructiveToolGuard_BashDefaultDeny verifies that the blocking guard
// defaults to denying ANY bash command — even a benign `ls` — because shell
// expansion makes a blacklist trivially bypassable for a security boundary.
func TestDestructiveToolGuard_BashDefaultDeny(t *testing.T) {
	rule := supervisor.DestructiveToolGuard()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "ls -la"},
	}

	if !rule.Match(event) {
		t.Error("bash should match guard (default-deny for shell commands)")
	}
}

// TestDestructiveToolGuard_BashBypassAttempt verifies that a shell-expansion
// bypass attempt (rm${IFS}-rf) is still caught — it's bash, so default-deny
// applies regardless of the specific command text.
func TestDestructiveToolGuard_BashBypassAttempt(t *testing.T) {
	rule := supervisor.DestructiveToolGuard()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "rm${IFS}-rf /important"},
	}

	if !rule.Match(event) {
		t.Error("bypass attempt should match guard (default-deny for bash)")
	}
}

func TestDestructiveToolGuard_Write(t *testing.T) {
	rule := supervisor.DestructiveToolGuard()

	event := trace.EffectEvent{
		Mode:      trace.Declaration,
		KindLabel: "write",
		SchemaRef: "yaah.tool.write.v1",
		Payload:   map[string]any{"path": "/tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("write should match guard")
	}

	iv := rule.Action(event)
	if iv == nil {
		t.Fatal("guard should produce intervention for write")
	}
	if iv.Type != supervisor.InterventionDeny {
		t.Errorf("expected deny, got %s", iv.Type)
	}
}

// TestHighErrorRateRule_MessageReportsSampledWindow pins the denominator: it
// must report the window that was evaluated, not len(results) after the buffer
// is reset to nil.
func TestHighErrorRateRule_MessageReportsSampledWindow(t *testing.T) {
	rule := supervisor.HighErrorRateRule(0.5, 6)

	var iv *supervisor.Intervention
	for i := 0; i < 3; i++ {
		event := trace.EffectEvent{
			TraceOwnerID: "sub:error-rate-message",
			Mode:         trace.Capture,
			KindLabel:    "bash:result",
			Payload:      map[string]any{"success": false},
		}
		rule.Match(event)
		iv = rule.Action(event)
	}
	if iv == nil {
		t.Fatal("expected an intervention after a 100% error window")
	}

	msg, _ := iv.Payload.(string)
	if !strings.Contains(msg, "(3/3 recent calls)") {
		t.Errorf("payload must report the sampled window, got %q", msg)
	}
}
