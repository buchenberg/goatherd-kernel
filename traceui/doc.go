// Package traceui serves a read-only HTTP projection of a trace store: the
// appended records as JSON, and - when an effect bus is attached - a live
// server-sent-events stream of the same events the Supervisor observes.
//
// It is the server half of the trace visualizer. The client half is a
// separate repository and consumes this contract:
//
//	GET /api/health
//	    {"status":"ok","bus":true,"facts":42}
//
//	GET /api/records?path=<ownerPathPrefix>&through=<ordinal>&mode=<both|captures|declarations>
//	    Returns the records visible under an owner path prefix. An empty
//	    path spans every owner. through defaults to no cutoff; mode
//	    defaults to both. Records carry id, digest, schema, mode, kind,
//	    owner, ordinal, context ref, witness ref, causal parents, and -
//	    under the trusted read this service uses - the retained payload.
//	    causal_edges carries the slice's graph; frontier, when non-null,
//	    is the read's resolved frontier.
//
//	GET /api/events
//	    Server-sent events, one "event: effect" per appended record while
//	    the stream is open. Requires a bus (embedded mode); a standalone
//	    viewer without a bus answers 503 and the client should poll
//	    /api/records instead. The bus is non-blocking: a stalled client
//	    drops events and repairs by re-reading /api/records, whose
//	    content-addressed ids make the merge trivial.
//
// Reads use the trusted internal context, so the payload visibility here is
// full: mount this behind whatever boundary your payloads deserve.
package traceui
