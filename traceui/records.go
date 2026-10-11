package traceui

import (
	"net/http"
	"strconv"

	"github.com/buchenberg/shepherd-kernel-go/trace"
)

// recordsResponse is the /api/records payload.
type recordsResponse struct {
	Visibility  string         `json:"visibility"`
	Mode        string         `json:"mode"`
	Owners      []ownerSummary `json:"owners"`
	Records     []recordDTO    `json:"records"`
	CausalEdges [][2]string    `json:"causal_edges"`
	Frontier    *frontierDTO   `json:"frontier"`
}

type ownerSummary struct {
	ID      string `json:"id"`
	Records int    `json:"records"`
}

// recordDTO is one visible record. Payload is present when the record is
// visible in full (the trusted read this service performs); hidden records
// carry their hidden reason instead.
type recordDTO struct {
	ID           string         `json:"id"`
	Digest       string         `json:"digest"`
	SchemaRef    string         `json:"schema_ref"`
	Mode         string         `json:"mode"`
	KindLabel    string         `json:"kind_label,omitempty"`
	Owner        string         `json:"owner"`
	Ordinal      int            `json:"ordinal"`
	ContextRef   string         `json:"context_ref,omitempty"`
	WitnessRef   string         `json:"witness_ref,omitempty"`
	CausedBy     []string       `json:"caused_by"`
	Payload      map[string]any `json:"payload,omitempty"`
	HiddenReason string         `json:"hidden_reason,omitempty"`
}

type frontierDTO struct {
	FrontierID  string `json:"frontier_id"`
	Owner       string `json:"owner"`
	ThroughFact string `json:"through_fact_id"`
	ThroughOrd  int    `json:"through_owner_ordinal"`
	Publisher   string `json:"publisher_owner_id"`
	CreatedBy   string `json:"created_by_fact_id"`
}

// handleRecords projects a slice of the store as JSON. path is an owner path
// prefix (empty spans all owners), through is an ordinal cutoff (0 = none),
// mode selects the record-mode filter.
func (s *Service) handleRecords(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	through := maxThrough
	if raw := r.URL.Query().Get("through"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "through must be a non-negative ordinal")
			return
		}
		through = n
	}
	mode, modeFilter, err := parseMode(r.URL.Query().Get("mode"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	slice, err := s.store.ReadPathPrefix(r.Context(), trace.TrustedReadContext, path, through, modeFilter)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "read: "+err.Error())
		return
	}

	resp := recordsResponse{
		Visibility:  string(slice.VisibilityProfile),
		Mode:        mode,
		Owners:      []ownerSummary{},
		Records:     []recordDTO{},
		CausalEdges: slice.CausalEdges,
	}
	// Arrays are never null: the client contract guarantees [] for empty.
	if resp.CausalEdges == nil {
		resp.CausalEdges = [][2]string{}
	}
	if slice.Frontier != nil {
		resp.Frontier = &frontierDTO{
			FrontierID:  slice.Frontier.FrontierID,
			Owner:       slice.Frontier.TargetTraceOwnerID,
			ThroughFact: slice.Frontier.ThroughFactID,
			ThroughOrd:  slice.Frontier.ThroughOwnerOrdinal,
			Publisher:   slice.Frontier.PublisherOwnerID,
			CreatedBy:   slice.Frontier.CreatedByFactID,
		}
	}
	for _, owner := range slice.OwnerPathOrder {
		resp.Owners = append(resp.Owners, ownerSummary{ID: owner, Records: len(slice.OwnerPaths[owner])})
	}
	for _, id := range slice.FactIDs() {
		vr, ok := slice.FactsByID[id]
		if !ok {
			continue
		}
		dto := recordDTO{ID: id}
		env := vr.GetEnvelope()
		dto.Digest = env.Digest
		dto.SchemaRef = env.SchemaRef
		dto.Mode = string(env.Mode)
		dto.WitnessRef = env.WitnessRef
		dto.CausedBy = env.CausedByIDs
		if view := vr.GetView(); view != nil {
			dto.Owner = view.TraceOwnerID
			dto.Ordinal = view.OwnerOrdinal
			dto.ContextRef = view.ContextRef
			dto.KindLabel = view.KindLabel
		}
		if rec, full := vr.(trace.Record); full {
			dto.Payload = rec.Body.Payload
		} else if shape, hidden := vr.(trace.RecordShape); hidden {
			dto.HiddenReason = shape.HiddenReason
		}
		resp.Records = append(resp.Records, dto)
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseMode(raw string) (string, trace.ModeFilter, error) {
	switch raw {
	case "", "both":
		return "both", trace.ModeBoth, nil
	case "captures", "captures_only":
		return "captures", trace.ModeCapturesOnly, nil
	case "declarations", "declarations_only":
		return "declarations", trace.ModeDeclarationsOnly, nil
	default:
		return "", "", errBadMode
	}
}

type badModeError struct{}

func (badModeError) Error() string {
	return "mode must be one of both, captures, declarations"
}

var errBadMode = badModeError{}
