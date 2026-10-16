package traceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/buchenberg/shepherd-kernel-go"
)

// eventDTO is the SSE wire form of trace.EffectEvent: a stable, explicitly
// named contract for the client repository.
type eventDTO struct {
	RecordID      string         `json:"record_id"`
	IntentID      string         `json:"intent_id"`
	TraceOwnerID  string         `json:"trace_owner_id"`
	Mode          string         `json:"mode"`
	SchemaRef     string         `json:"schema_ref"`
	KindLabel     string         `json:"kind_label"`
	Payload       map[string]any `json:"payload"`
	CausalParents []string       `json:"causal_parents"`
	Timestamp     time.Time      `json:"timestamp"`
}

func eventDTOFrom(e trace.EffectEvent) eventDTO {
	causalParents := e.CausalParents
	// Arrays are never null: a root effect has no causal parents and
	// marshals [], so SSE consumers never guard a null.
	if causalParents == nil {
		causalParents = []string{}
	}
	return eventDTO{
		RecordID:      e.RecordID,
		IntentID:      e.IntentID,
		TraceOwnerID:  e.TraceOwnerID,
		Mode:          string(e.Mode),
		SchemaRef:     e.SchemaRef,
		KindLabel:     e.KindLabel,
		Payload:       e.Payload,
		CausalParents: causalParents,
		Timestamp:     e.Timestamp,
	}
}

// heartbeatInterval keeps proxies from closing an idle SSE stream.
const heartbeatInterval = 15 * time.Second

// handleEvents streams one SSE "effect" event per appended record. The bus
// publishes non-blocking, so a slow client drops events rather than
// stalling appends; the client backfills with /api/records.
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		httpError(w, http.StatusServiceUnavailable, "no bus attached: embed the service in the writer process, or poll /api/records")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// The id is process-unique, not just service-unique: two services can
	// share one bus, and a colliding id would close the other stream's
	// subscription.
	subID := fmt.Sprintf("traceui-%d", subSeq.Add(1))
	ch := s.bus.Subscribe(subID)
	defer s.bus.Unsubscribe(subID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: hello\ndata: {\"subscription\":\""+subID+"\"}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(eventDTOFrom(e))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: effect\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
