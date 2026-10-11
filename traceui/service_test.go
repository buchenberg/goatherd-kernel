package traceui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-kernel-go/trace"
)

func newMemStore(t *testing.T) *trace.SQLiteTraceStore {
	t.Helper()
	store, err := trace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

var trustedAppend = trace.AppendContext{
	ActorRef:             "runtime:traceui-test",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

func appendDrafts(t *testing.T, store *trace.SQLiteTraceStore, intent, owner string, drafts ...trace.RecordDraft) {
	t.Helper()
	if _, err := store.Append(context.Background(), trustedAppend, trace.AppendBatch{
		AppendIntentID: intent,
		Groups:         []trace.AppendGroup{{TraceOwnerID: owner, FactDrafts: drafts}},
	}); err != nil {
		t.Fatalf("Append(%s): %v", intent, err)
	}
}

func getJSON(t *testing.T, url string, into any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp
}

func TestHealth(t *testing.T) {
	store := newMemStore(t)
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	var health struct {
		Status string `json:"status"`
		Bus    bool   `json:"bus"`
		Facts  int    `json:"facts"`
	}
	getJSON(t, srv.URL+"/api/health", &health)
	if health.Status != "ok" || health.Bus || health.Facts != 0 {
		t.Errorf("health = %+v, want ok/no-bus/0", health)
	}
}

func TestRecords(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:1", "agent:main",
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.write.v1", KindLabel: "write", Payload: map[string]any{"path": "a.txt"}},
		trace.RecordDraft{Mode: trace.Capture, SchemaRef: "example.write.applied.v1", KindLabel: "write-applied", Payload: map[string]any{"ok": true}},
	)
	appendDrafts(t, store, "intent:2", "agent:main:child",
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.exec.v1", KindLabel: "exec"},
	)
	svc, _ := New(Config{Store: store})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	var resp recordsResponse
	getJSON(t, srv.URL+"/api/records", &resp)
	// The slice carries witness records too (the store retains them on the
	// owner path); count by what the test appended.
	var writes, witnesses int
	var first recordDTO
	lastOrdinal := map[string]int{}
	for _, rec := range resp.Records {
		if rec.SchemaRef == trace.RootWitnessSchemaRef || rec.SchemaRef == trace.WitnessSchemaRef {
			witnesses++
			continue
		}
		writes++
		if rec.SchemaRef == "example.write.v1" {
			first = rec
		}
		// Ordinals are per owner path; each owner's records ascend in
		// read order.
		if last, seen := lastOrdinal[rec.Owner]; seen && rec.Ordinal <= last {
			t.Errorf("ordinals not ascending for %s: %d after %d", rec.Owner, rec.Ordinal, last)
		}
		lastOrdinal[rec.Owner] = rec.Ordinal
	}
	if writes != 3 {
		t.Fatalf("appended records = %d, want 3 (witnesses: %d)", writes, witnesses)
	}
	if witnesses == 0 {
		t.Error("no witness records in the slice, want the witness chain visible")
	}
	if first.ID == "" || first.Owner != "agent:main" {
		t.Errorf("write record = %+v", first)
	}
	if first.Payload["path"] != "a.txt" {
		t.Errorf("payload = %+v, want retained payload under trusted read", first.Payload)
	}
	// Owners include the witness owner path (kernel:witness), where the
	// store retains witness records.
	if len(resp.Owners) != 3 {
		t.Errorf("owners = %+v, want 3 owner paths including kernel:witness", resp.Owners)
	}

	// Path-prefix reads scope to a subtree.
	getJSON(t, srv.URL+"/api/records?path=agent:main:child", &resp)
	var childWrites int
	for _, rec := range resp.Records {
		if rec.Owner != "agent:main:child" {
			t.Errorf("path-scoped record leaked from owner %s", rec.Owner)
		}
		if rec.SchemaRef == "example.exec.v1" {
			childWrites++
		}
	}
	if childWrites != 1 {
		t.Errorf("path-scoped appended records = %d, want 1", childWrites)
	}

	// Mode filters: the two declarations plus their witnesses as retained
	// declaration-mode records - the filter holds for every record returned.
	getJSON(t, srv.URL+"/api/records?mode=declarations", &resp)
	for _, rec := range resp.Records {
		if rec.Mode != string(trace.Declaration) {
			t.Errorf("declarations filter returned %s record", rec.Mode)
		}
	}
	getJSON(t, srv.URL+"/api/records?mode=captures", &resp)
	for _, rec := range resp.Records {
		if rec.Mode != string(trace.Capture) {
			t.Errorf("captures filter returned %s record", rec.Mode)
		}
	}

	// Bad params fail loudly, not silently.
	respHTTP, _ := http.Get(srv.URL + "/api/records?mode=bogus")
	respHTTP.Body.Close()
	if respHTTP.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus mode status = %d, want 400", respHTTP.StatusCode)
	}
}

func TestEventsWithoutBus(t *testing.T) {
	store := newMemStore(t)
	svc, _ := New(Config{Store: store})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestEventsStream(t *testing.T) {
	store := newMemStore(t)
	bus := trace.NewEffectBus(16)
	store.WithBus(bus)
	svc, _ := New(Config{Store: store, Bus: bus})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := make(chan eventDTO, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(resp.Body)
		var evt, data string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				evt = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if evt == "effect" {
					var dto eventDTO
					if err := json.Unmarshal([]byte(data), &dto); err == nil {
						events <- dto
					}
				}
				evt, data = "", ""
			}
		}
	}()

	appendDrafts(t, store, "intent:live", "agent:main",
		trace.RecordDraft{Mode: trace.Capture, SchemaRef: "example.read.v1", KindLabel: "read", Payload: map[string]any{"n": 1}},
	)

	select {
	case dto := <-events:
		if dto.KindLabel != "read" || dto.TraceOwnerID != "agent:main" || dto.Payload["n"] != float64(1) {
			t.Errorf("event = %+v", dto)
		}
		if dto.RecordID == "" || dto.Timestamp.IsZero() {
			t.Errorf("event missing id or timestamp: %+v", dto)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE event within 5s")
	}

	// Disconnect must unsubscribe: the bus's subscriber count returns to
	// its pre-stream value once the request context is cancelled.
	resp.Body.Close()
	<-readerDone
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("subscriber count = %d after disconnect, want 0", bus.SubscriberCount())
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New without store should fail")
	}
}
