package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// mustPanic runs fn, failing the test unless it panicked with a message
// containing want.
func mustPanic(t *testing.T, want, label string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("%s: did not panic", label)
			return
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, want) {
			t.Errorf("%s: panic = %v, want it to mention %q", label, r, want)
		}
	}()
	fn()
}

// The registry-mechanics tests run against a private registry instance, so
// they stay independent of which backends are linked in. The git backend's
// OpenSandbox integration lives with the backend, in sandbox/git.

func TestSandboxRegistry_InstanceRegistration(t *testing.T) {
	r := &sandboxRegistry{factories: map[string]SandboxFactory{}}

	// The programmer-error guards.
	mustPanic(t, "empty backend name", "empty name", func() { r.register("", func(any) (Sandbox, error) { return nil, nil }) })
	mustPanic(t, "nil factory", "nil factory", func() { r.register("x", nil) })
	r.register("x", func(any) (Sandbox, error) { return nil, nil })
	mustPanic(t, "already registered", "duplicate", func() { r.register("x", func(any) (Sandbox, error) { return nil, nil }) })

	// Unknown backends fail with the sentinel and list what exists.
	_, err := r.open("nope", nil)
	var unknown *UnknownSandboxBackendError
	if !errors.As(err, &unknown) || !errors.Is(err, ErrUnknownSandboxBackend) {
		t.Fatalf("open unknown: %v, want UnknownSandboxBackendError behind the sentinel", err)
	}
	if unknown.Backend != "nope" || len(unknown.Available) != 1 || unknown.Available[0] != "x" {
		t.Errorf("unknown error details: %+v", unknown)
	}
	if !strings.Contains(err.Error(), "x") {
		t.Errorf("error text %q should list the available backend", err.Error())
	}
}

func TestSandboxRegistry_NilFactoryResultFailsAtTheSeam(t *testing.T) {
	r := &sandboxRegistry{factories: map[string]SandboxFactory{}}
	r.register("broken", func(any) (Sandbox, error) { return nil, nil })

	_, err := r.open("broken", nil)
	if err == nil || !strings.Contains(err.Error(), "nil sandbox") {
		t.Fatalf("open with a (nil, nil) factory: %v, want it to fail at the construction seam", err)
	}
}
