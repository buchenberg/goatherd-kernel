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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/buchenberg/shepherd-kernel-go/trace"
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
	// Refuse to create a database: a viewer pointed at a wrong path should
	// fail loudly, not show an empty trace of a session that never existed.
	if _, err := os.Stat(*db); err != nil {
		log.Fatalf("traceui: %v", err)
	}

	store, err := trace.NewSQLiteTraceStore(*db)
	if err != nil {
		log.Fatalf("traceui: open %s: %v", *db, err)
	}
	defer store.Close(context.Background())

	svc, err := traceui.New(traceui.Config{Store: store})
	if err != nil {
		log.Fatalf("traceui: %v", err)
	}

	log.Printf("traceui: serving %s on http://%s (no bus: poll /api/records)", *db, *addr)
	log.Fatal(http.ListenAndServe(*addr, svc.Handler()))
}
