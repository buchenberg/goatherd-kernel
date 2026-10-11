package trace

// Materialization contract types shared by the trace store's completed-intent
// ledger and the substrate/materialize components. They live in the trace
// component because the ledger's receipt_json is serialized from
// MaterializationReceipt and must stay cross-readable with the Python
// reference.

// MaterializationOutcome is the result vocabulary a substrate reports.
type MaterializationOutcome string

const (
	// MaterializationSuccess means every declaration was applied and every
	// intended capture is present in the result.
	MaterializationSuccess MaterializationOutcome = "success"
	// MaterializationCleanFailure means nothing was applied and the failure
	// reason explains why. No captures are emitted — a substrate claiming
	// clean_failure while emitting capture drafts is rejected by dispatch,
	// because the two claims contradict each other. A substrate may only
	// claim clean_failure when it did not attempt a world-side mutation:
	// a failure after a mutation was attempted is split_state, because the
	// world may have changed without a capture.
	MaterializationCleanFailure MaterializationOutcome = "clean_failure"
	// MaterializationSplitState means the batch did not apply cleanly: the
	// captures and anchors describe only the records that verifiably
	// landed, and the world-side state of the failed record(s) is uncertain
	// — the failure reason names them.
	MaterializationSplitState MaterializationOutcome = "split_state"
)

// MaterializationResult is the substrate-produced result before the kernel
// appends captures. Substrates never append themselves: dispatch owns the
// append (the materialize orchestrator), which keeps substrates testable
// without a store.
type MaterializationResult struct {
	Outcome          MaterializationOutcome
	CaptureDrafts    []RecordDraft
	FailureReason     string
	WorldSideAnchors []map[string]any
}

// MaterializationReceipt is the kernel materialize transition receipt handed
// back to the caller and stored in the completed-intent ledger. Its field set
// mirrors Python's MaterializationReceipt exactly, because the ledger's
// receipt_json is serialized from it and must stay cross-readable.
type MaterializationReceipt struct {
	Outcome           MaterializationOutcome
	SubstrateRef      string
	TargetRecordIDs   []string
	ProducedRecordIDs []string
	FailureReason     string
	WorldSideAnchors  []map[string]any
}
