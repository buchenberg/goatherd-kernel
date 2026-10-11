package traceui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/buchenberg/shepherd-kernel-go/trace"
)

// maxThrough is the default ordinal cutoff: effectively none, mirroring the
// materialize orchestrator's MaxOwnerOrdinal without importing it (which
// would pull the substrate tree into a viewer).
const maxThrough = 1<<63 - 1

// Config builds a Service.
type Config struct {
	// Store is the trace store to project. Required.
	Store *trace.SQLiteTraceStore

	// Bus, when set, enables the live /api/events stream. It must be the
	// same bus the store publishes through (Store.WithBus), which happens
	// naturally in embedded mode: construct the store, attach the bus, and
	// hand both to the service.
	Bus *trace.EffectBus

	// AllowedOrigins restricts cross-origin use (the client may live in
	// another repo and serve from another port in development). nil allows
	// every origin, which is the dev-tool default; the endpoints are
	// unauthenticated reads, so a restrictive list adds no protection -
	// restrict with a reverse proxy if the port is reachable.
	AllowedOrigins []string
}

// Service is the read-only HTTP projection of a trace store.
type Service struct {
	store   *trace.SQLiteTraceStore
	bus     *trace.EffectBus
	origins []string
	subSeq  atomic.Uint64
}

// New validates the configuration and returns the service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("traceui: store is required")
	}
	return &Service{
		store:   cfg.Store,
		bus:     cfg.Bus,
		origins: cfg.AllowedOrigins,
	}, nil
}

// Handler returns the service's routes.
func (s *Service) Handler() http.Handler {
	// Plain-path registration with explicit method checks: the module's
	// go directive is 1.21, where ServeMux has no method patterns.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.get(s.handleHealth))
	mux.HandleFunc("/api/records", s.get(s.handleRecords))
	mux.HandleFunc("/api/events", s.get(s.handleEvents))
	return s.cors(mux)
}

// get wraps a handler with a GET-only guard.
func (s *Service) get(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httpError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		next(w, r)
	}
}

// cors applies the cross-origin policy and disables response caching: a
// trace is append-only, and a cached records response is a stale one.
func (s *Service) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(s.origins) == 0 {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			origin := r.Header.Get("Origin")
			for _, allowed := range s.origins {
				if origin == allowed {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					break
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	facts, err := s.store.FactCount(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "fact count: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"bus":    s.bus != nil,
		"facts":  facts,
	})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
