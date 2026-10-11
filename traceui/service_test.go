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

	// A path prefix selects the subtree, not the exact path: agent:main
	// spans agent:main and its :child descendant, and nothing else.
	getJSON(t, srv.URL+"/api/records?path=agent:main", &resp)
	ownersSeen := map[string]bool{}
	for _, rec := range resp.Records {
		if rec.Owner != "agent:main" && rec.Owner != "agent:main:child" {
			t.Errorf("subtree read leaked record from owner %s", rec.Owner)
		}
		ownersSeen[rec.Owner] = true
	}
	if !ownersSeen["agent:main"] || !ownersSeen["agent:main:child"] {
		t.Errorf("subtree read owners = %v, want agent:main and agent:main:child", ownersSeen)
	}
	// The prefix is delimiter-safe: a path that merely starts with the
	// prefix string is not part of the subtree.
	getJSON(t, srv.URL+"/api/records?path=agent:mainly", &resp)
	if len(resp.Records) != 0 {
		t.Errorf("agent:mainly matched %d records, want 0", len(resp.Records))
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
	respHTTP, err := http.Get(srv.URL + "/api/records?mode=bogus")
	if err != nil {
		t.Fatalf("GET bogus mode: %v", err)
	}
	respHTTP.Body.Close()
	if respHTTP.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus mode status = %d, want 400", respHTTP.StatusCode)
	}
}

func TestRecordsThroughCutoff(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:cut", "agent:main",
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.one.v1", KindLabel: "one"},
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.two.v1", KindLabel: "two"},
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.three.v1", KindLabel: "three"},
	)
	svc, _ := New(Config{Store: store})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	var resp recordsResponse
	getJSON(t, srv.URL+"/api/records?path=agent:main&through=1", &resp)
	if len(resp.Records) != 2 {
		t.Errorf("through=1 records = %d, want 2", len(resp.Records))
	}
	for _, rec := range resp.Records {
		if rec.Ordinal > 1 {
			t.Errorf("through=1 returned ordinal %d", rec.Ordinal)
		}
	}

	// through is inclusive: 0 keeps each owner's first record, it is not
	// the "no cutoff" default (absence is).
	getJSON(t, srv.URL+"/api/records?path=agent:main&through=0", &resp)
	if len(resp.Records) != 1 || resp.Records[0].Ordinal != 0 {
		t.Errorf("through=0 records = %+v, want the single ordinal-0 record", resp.Records)
	}

	getJSON(t, srv.URL+"/api/records?path=agent:main", &resp)
	if len(resp.Records) != 3 {
		t.Errorf("uncut records = %d, want 3", len(resp.Records))
	}

	// Bad cutoffs fail loudly.
	for _, bad := range []string{"through=abc", "through=-1"} {
		respHTTP, err := http.Get(srv.URL + "/api/records?" + bad)
		if err != nil {
			t.Fatalf("GET %s: %v", bad, err)
		}
		respHTTP.Body.Close()
		if respHTTP.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", bad, respHTTP.StatusCode)
		}
	}
}

func TestRecordsPerOwnerPathMetadata(t *testing.T) {
	store := newMemStore(t)
	// The same content-addressed record retained on two owner paths: the
	// projection must show it once per path, each with that path's own
	// owner and ordinal, not one entry labeled by whichever path the
	// store's id-keyed slice happened to load last.
	same := trace.RecordDraft{Mode: trace.Capture, SchemaRef: "example.shared.v1", KindLabel: "shared", Payload: map[string]any{"n": 1}}
	appendDrafts(t, store, "intent:owner-a", "owner:a", same)
	appendDrafts(t, store, "intent:owner-b", "owner:b", same)
	svc, _ := New(Config{Store: store})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	var resp recordsResponse
	getJSON(t, srv.URL+"/api/records?path=owner", &resp)
	byOwner := map[string]recordDTO{}
	for _, rec := range resp.Records {
		if rec.SchemaRef != "example.shared.v1" {
			continue
		}
		if prev, seen := byOwner[rec.Owner]; seen && prev.ID != rec.ID {
			t.Errorf("owner %s has two different shared records: %s, %s", rec.Owner, prev.ID, rec.ID)
		}
		byOwner[rec.Owner] = rec
	}
	if len(byOwner) != 2 {
		t.Fatalf("shared record owners = %v, want owner:a and owner:b", byOwner)
	}
	for owner, rec := range byOwner {
		if rec.Owner != owner || rec.Ordinal != 0 {
			t.Errorf("shared record on %s = %+v, want that owner's ordinal-0 entry", owner, rec)
		}
	}
	// Scoping to one path keeps only that path's occurrence, with its own
	// metadata.
	getJSON(t, srv.URL+"/api/records?path=owner:a", &resp)
	shared := 0
	for _, rec := range resp.Records {
		if rec.SchemaRef != "example.shared.v1" {
			continue
		}
		shared++
		if rec.Owner != "owner:a" || rec.Ordinal != 0 {
			t.Errorf("owner:a occurrence = %+v, want owner:a ordinal 0", rec)
		}
	}
	if shared != 1 {
		t.Errorf("path=owner:a shared records = %d, want 1", shared)
	}
}

func TestRecordsKindLabelDefaultsToSchemaRef(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:unlabeled", "agent:main",
		trace.RecordDraft{Mode: trace.Declaration, SchemaRef: "example.unlabeled.v1"},
	)
	svc, _ := New(Config{Store: store})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	var resp recordsResponse
	getJSON(t, srv.URL+"/api/records?path=agent:main", &resp)
	for _, rec := range resp.Records {
		if rec.SchemaRef == "example.unlabeled.v1" && rec.KindLabel != rec.SchemaRef {
			t.Errorf("kind_label = %q, want the schema ref %q the store persisted", rec.KindLabel, rec.SchemaRef)
		}
	}
}

func TestCORS(t *testing.T) {
	store := newMemStore(t)
	svc, _ := New(Config{Store: store, AllowedOrigins: []string{"http://localhost:5173"}})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	// A preflight gets the access-control answers the browser needs,
	// instead of the GET-only guard's 405.
	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/api/records", nil)
	if err != nil {
		t.Fatalf("new preflight request: %v", err)
	}
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "Last-Event-ID")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("preflight allow-origin = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "GET, OPTIONS" {
		t.Errorf("preflight allow-methods = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Last-Event-ID" {
		t.Errorf("preflight allow-headers = %q", got)
	}

	// An unlisted origin is not granted access.
	req, err = http.NewRequest(http.MethodOptions, srv.URL+"/api/records", nil)
	if err != nil {
		t.Fatalf("new preflight request: %v", err)
	}
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin allow-origin = %q, want none", got)
	}

	// Allowed-origin GETs are annotated, and the response varies by Origin
	// so caches must not reuse one origin's answer for another.
	get, err := http.NewRequest(http.MethodGet, srv.URL+"/api/health", nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	get.Header.Set("Origin", "http://localhost:5173")
	resp, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatalf("GET with origin: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("allow-origin = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("vary = %q, want Origin", got)
	}

	// Without configured origins everything is allowed with a wildcard.
	open, _ := New(Config{Store: newMemStore(t)})
	openSrv := httptest.NewServer(open.Handler())
	t.Cleanup(openSrv.Close)
	resp, err = http.Get(openSrv.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET open service: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow-origin = %q, want *", got)
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

func TestEventsMetadataMatchesPersistedRecords(t *testing.T) {
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

	// An unlabeled root effect: the event must carry the persisted record
	// metadata — the schema-ref kind label — and its causal_parents array
	// must never be null.
	appendDrafts(t, store, "intent:unlabeled", "agent:main",
		trace.RecordDraft{Mode: trace.Capture, SchemaRef: "example.unlabeled.v1"},
	)

	type sseEvent struct {
		name string
		data string
	}
	events := make(chan sseEvent, 4)
	go func() {
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
				if evt != "" {
					events <- sseEvent{evt, data}
				}
				evt, data = "", ""
			}
		}
	}()

	for {
		select {
		case e := <-events:
			if e.name != "effect" {
				continue
			}
			var dto eventDTO
			if err := json.Unmarshal([]byte(e.data), &dto); err != nil {
				t.Fatalf("decode effect event %q: %v", e.data, err)
			}
			if dto.SchemaRef != "example.unlabeled.v1" {
				continue
			}
			if dto.KindLabel != dto.SchemaRef {
				t.Errorf("kind_label = %q, want the persisted schema-ref default %q", dto.KindLabel, dto.SchemaRef)
			}
			if !strings.Contains(e.data, `"causal_parents":[]`) {
				t.Errorf("root effect marshals %q, want a non-null empty causal_parents array", e.data)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no effect event within 5s")
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New without store should fail")
	}
}
