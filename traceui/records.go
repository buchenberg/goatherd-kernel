package traceui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/buchenberg/shepherd-kernel-go"
)

// recordsResponse is the /api/records payload.
type recordsResponse struct {
	Visibility  string         `json:"visibility"`
	Mode        string         `json:"mode"`
	Owners      []ownerSummary `json:"owners"`
	Records     []recordDTO    `json:"records"`
	CausalEdges [][2]string    `json:"causal_edges"`
}

type ownerSummary struct {
	ID      string `json:"id"`
	Records int    `json:"records"`
}

// recordDTO is one visible record on one owner path. Payload is present when
// the record is visible in full (the trusted read this service performs);
// hidden records carry their hidden reason instead.
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

// handleRecords projects a slice of the store as JSON. path selects an owner
// path subtree (empty spans all owners), through is an inclusive per-owner
// ordinal cutoff (absent means no cutoff), mode selects the record-mode
// filter.
//
// Every selected owner path is read on its own rather than flattened through
// one slice: the store retains a content-addressed record on multiple owner
// paths, and a single slice keys records by id, so a merged read would
// label the record with whichever path was loaded last. Per-path reads keep
// each occurrence's own owner, ordinal, and kind label — a record appears
// once per owner path that retains it.
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

	// One unscoped read enumerates the owner paths; each path in the
	// selected subtree is then read individually.
	all, err := s.store.ReadOwnerPrefix(r.Context(), trace.TrustedReadContext, "", through, modeFilter)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "read: "+err.Error())
		return
	}

	resp := recordsResponse{
		Visibility:  string(all.VisibilityProfile),
		Mode:        mode,
		Owners:      []ownerSummary{},
		Records:     []recordDTO{},
		CausalEdges: [][2]string{},
	}
	seenEdges := make(map[[2]string]bool)
	for _, owner := range all.OwnerPathOrder {
		if !pathMatches(path, owner) {
			continue
		}
		slice, err := s.store.ReadOwnerPrefix(r.Context(), trace.TrustedReadContext, owner, through, modeFilter)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "read "+owner+": "+err.Error())
			return
		}
		resp.Owners = append(resp.Owners, ownerSummary{ID: owner, Records: len(slice.OwnerPaths[owner])})
		for _, id := range slice.FactIDs() {
			vr, ok := slice.FactsByID[id]
			if !ok {
				continue
			}
			resp.Records = append(resp.Records, recordDTOFrom(id, vr))
		}
		for _, edge := range slice.CausalEdges {
			if !seenEdges[edge] {
				seenEdges[edge] = true
				resp.CausalEdges = append(resp.CausalEdges, edge)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// recordDTOFrom projects one visible record. CausedBy is normalized so the
// array contract holds: a record without causal parents carries [], never
// null.
func recordDTOFrom(id string, vr trace.VisibleRecord) recordDTO {
	dto := recordDTO{ID: id}
	env := vr.GetEnvelope()
	dto.Digest = env.Digest
	dto.SchemaRef = env.SchemaRef
	dto.Mode = string(env.Mode)
	dto.WitnessRef = env.WitnessRef
	dto.CausedBy = env.CausedByIDs
	if dto.CausedBy == nil {
		dto.CausedBy = []string{}
	}
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
	return dto
}

// pathMatches reports whether owner names a path inside the subtree rooted
// at prefix: the path itself, or a ":"-delimited descendant of it. The
// delimiter keeps a prefix from bleeding into unrelated paths — agent:main
// matches agent:main:child but not agent:mainly.
func pathMatches(prefix, owner string) bool {
	return prefix == "" || owner == prefix || strings.HasPrefix(owner, prefix+":")
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
