// Command traceui is a standalone read-only viewer for a shepherd trace
// database: it serves the traceui package's HTTP API over an existing trace
// file, for pointing a browser client at a session after (or during) a run.
//
// Because the effect bus lives in the writer's process, a standalone viewer
// has no bus: /api/events answers 503 and clients should poll /api/records.
// For a live stream, embed the traceui.Service in the writer instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/buchenberg/shepherd-kernel-go"
	"github.com/buchenberg/shepherd-kernel-go/traceui"
)

func main() {
	db := flag.String("db", "", "path to the trace database (required)")
	addr := flag.String("addr", "127.0.0.1:8321", "listen address")
	flag.Parse()

	if *db == "" {
		fmt.Fprintln(os.Stderr, "traceui: -db is required")
		flag.Usage()
		os.Exit(2)
	}

	// Read-only opener: no journal-mode pragma, no schema creation, and a
	// schema check on the existing file — a viewer pointed at a wrong path
	// fails loudly, never modifies a valid trace, and never invents an
	// empty one.
	store, err := trace.OpenSQLiteTraceStoreReadOnly(*db)
	if err != nil {
		log.Fatalf("traceui: open %s: %v", *db, err)
	}
	defer store.Close(context.Background())

	svc, err := traceui.New(traceui.Config{Store: store})
	if err != nil {
		log.Fatalf("traceui: %v", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: svc.Handler(),
		// The header timeout bounds slow clients without bounding the
		// responses themselves: /api/events streams for as long as the
		// client stays connected, so whole-request read/write timeouts
		// would cut healthy streams.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("traceui: serving %s on http://%s (no bus: poll /api/records)", *db, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("traceui: serve: %v", err)
	}
}
