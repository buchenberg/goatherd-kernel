package trace

// WorkspaceState is the backend-neutral description of a sandbox workspace:
// the state Capture produces and Apply consumes. It lives in the trace
// component because it is trace-recordable data — its Digest exists for trace
// records and drift detection — and both the sandbox and the runtime layer
// consume it through the public aliases.
type WorkspaceState struct {
	// Backend identifies the backend that produced the state ("git",
	// "containerd", ...). Apply rejects a state from a different backend.
	Backend string
	// Revision is an optional backend timeline position: the git HEAD SHA for
	// the git backend, a snapshot key for a snapshot-backed backend.
	Revision string
	// Data carries the backend-specific payload.
	Data map[string]any
}

// Digest returns the kernel canonical digest of the state, for trace records
// and drift detection. It is deterministic: CanonicalDigest sorts keys at every
// nesting level, so two states with the same content digest identically
// regardless of map iteration order.
//
// It returns an error only if Data contains a value that cannot be canonically
// marshaled. Callers recording a trace treat a digest failure as advisory.
func (ws WorkspaceState) Digest() (string, error) {
	return CanonicalDigest(map[string]any{
		"backend":  ws.Backend,
		"revision": ws.Revision,
		"data":     ws.Data,
	})
}
