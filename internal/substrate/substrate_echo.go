package substrate

// EchoSubstrate (plan 03 §3): a test substrate that turns declarations into
// same-schema captures. It is both the parity fixture for the conformance
// story and the reference for how a minimal substrate behaves — Python's
// EchoSubstrate is the same double.

import (
	"context"

	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// EchoSubstrate echoes declaration payloads into capture drafts.
type EchoSubstrate struct {
	Ref           string
	Declarations  []string
	SubstrateCont trace.Containment
}

// NewEchoSubstrate builds an echo substrate accepting the given declaration
// schemas, contained by default (matching Python's dataclass default).
func NewEchoSubstrate(ref string, declarationSchemas ...string) *EchoSubstrate {
	return &EchoSubstrate{
		Ref:           ref,
		Declarations:  declarationSchemas,
		SubstrateCont: trace.ContainContained,
	}
}

func (e *EchoSubstrate) SubstrateRef() string           { return e.Ref }
func (e *EchoSubstrate) DeclarationSchemas() []string   { return e.Declarations }
func (e *EchoSubstrate) CaptureSchemas() []string       { return e.Declarations }
func (e *EchoSubstrate) Containment() trace.Containment { return e.SubstrateCont }

// Materialize copies each declaration into a same-schema capture draft,
// caused by the declaration's own record id. The kind label mirrors the
// declaration's fact kind; records read through the store always carry one.
func (e *EchoSubstrate) Materialize(_ context.Context, records []trace.Record) (trace.MaterializationResult, error) {
	captures := make([]trace.RecordDraft, 0, len(records))
	for _, record := range records {
		kind := ""
		if record.View != nil {
			kind = record.View.KindLabel
		}
		captures = append(captures, trace.RecordDraft{
			Mode:            trace.Capture,
			SchemaRef:       record.Envelope.SchemaRef,
			KindLabel:       kind,
			Payload:         trace.CopyMap(record.Body.Payload),
			CausedByFactIDs: []string{record.Envelope.RecordID},
		})
	}
	return trace.MaterializationResult{Outcome: trace.MaterializationSuccess, CaptureDrafts: captures}, nil
}
