package substrate

// Minimal vNext substrate contract and registry (plan 03 §1), mirroring
// shepherd2/src/shepherd2/vnext/substrates.py.
//
// vNext is outside the ABI freeze, so the Go shape has latitude — but the
// outcome vocabulary, the substrate refs and the schema names below are
// cross-language contract surface: Python's test_materialize.py and the Go
// materialize vectors pin them.
//
// The outcome vocabulary mirrors the reference exactly: "success",
// "clean_failure", "split_state". The plan sketch's "succeeded | failed |
// partially_applied" was a guess made before the reference was read; where they
// disagree, the Python implementation decides (PARITY-PLAN §2, vectors over
// prose).

import (
	"context"
	"errors"
	"fmt"
	trace "github.com/buchenberg/shepherd-kernel-go/internal/trace"
	"reflect"
)

// ErrUnknownSubstrate is the sentinel behind UnknownSubstrateError, so
// callers can errors.Is(err, ErrUnknownSubstrate) without matching strings.
var ErrUnknownSubstrate = errors.New("shepherd: unknown substrate")

// SubstrateError is raised for substrate registration or dispatch failures.
// It mirrors Python's SubstrateError (RuntimeError subclass).
type SubstrateError struct {
	msg string
}

func (e *SubstrateError) Error() string { return e.msg }

// UnknownSubstrateError is raised when materialize dispatch names a
// substrate that is not registered.
type UnknownSubstrateError struct {
	substrateRef string
}

func (e *UnknownSubstrateError) Error() string {
	return fmt.Sprintf("unknown substrate: %s", e.substrateRef)
}

func (e *UnknownSubstrateError) Is(target error) bool { return target == ErrUnknownSubstrate }

// Substrate is a registered unit of declaration materialization.
//
// DeclarationSchemas and CaptureSchemas are treated as sets; a duplicate
// entry is harmless. Materialize receives the selected declaration records
// (already retained in the store, addressed by the request) and returns
// capture drafts; it never appends them itself.
type Substrate interface {
	// SubstrateRef identifies the KIND of substrate ("kv.sqlite.local.v1",
	// "workspace.sandbox.v1"). Backend details belong in capture payloads.
	SubstrateRef() string
	// DeclarationSchemas lists the intent schemas this substrate accepts.
	DeclarationSchemas() []string
	// CaptureSchemas lists the receipt schemas it emits.
	CaptureSchemas() []string
	// Containment is the witness-level containment classification stamped
	// into the retained context of the materialization captures.
	Containment() trace.Containment
	// Materialize applies the declarations and returns capture drafts.
	Materialize(ctx context.Context, records []trace.Record) (trace.MaterializationResult, error)
}

// SubstrateRegistry is a fail-closed process-local substrate registry.
// Registering a second, different substrate under an existing ref is an
// error; registering the same substrate twice is a no-op (Python compares
// identity, Go compares interface equality, which is identity for pointers).
// A non-comparable implementation (a value whose struct embeds a map, say)
// cannot be identity-compared without panicking, so it fails closed on
// re-registration instead — see sameSubstrate.
type SubstrateRegistry struct {
	substrates map[string]Substrate
}

// NewSubstrateRegistry returns an empty registry.
func NewSubstrateRegistry() *SubstrateRegistry {
	return &SubstrateRegistry{substrates: map[string]Substrate{}}
}

// Register adds a substrate under its ref.
func (r *SubstrateRegistry) Register(s Substrate) error {
	ref := s.SubstrateRef()
	if existing, ok := r.substrates[ref]; ok && !sameSubstrate(existing, s) {
		return &SubstrateError{fmt.Sprintf("substrate %q is already registered", ref)}
	}
	r.substrates[ref] = s
	return nil
}

// sameSubstrate reports whether two registered substrates are the same
// instance. Interface equality is identity for the intended pointer
// implementations, but it panics when the dynamic type is not comparable —
// for example a Substrate implemented on a value-typed struct containing a
// map. Such a value cannot be identity-checked, so it is treated as
// different and fails closed; implement Substrate on a pointer to get the
// Python no-op re-registration semantics.
func sameSubstrate(a, b Substrate) bool {
	t := reflect.TypeOf(a)
	if t == nil || t != reflect.TypeOf(b) || !t.Comparable() {
		return false
	}
	return a == b
}

// Get returns the substrate registered under ref.
func (r *SubstrateRegistry) Get(substrateRef string) (Substrate, error) {
	if s, ok := r.substrates[substrateRef]; ok {
		return s, nil
	}
	return nil, &UnknownSubstrateError{substrateRef: substrateRef}
}
