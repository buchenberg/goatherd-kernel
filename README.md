# goatherd-kernel

Go implementation of the [Shepherd](https://arxiv.org/abs/2605.10913) trace kernel: a content-addressed, append-only, causally-linked execution trace store over SQLite, the real-time effect bus, and the read-only trace UI. It implements `shepherd.kernel.abi.v0` — the same frozen ABI as the Python reference (`shepherd2`) — and produces byte-identical digests from the same golden test vectors.

> **Module scope.** This module is the **kernel**: the record model, canonical JSON, the SQLite store, the effect bus, and the execution/history projections. The trace package is the **module root** — `import "github.com/buchenberg/goatherd-kernel"` is package `trace` — with `traceui` (the read-only HTTP projection) and `testkit` (shared test support) as subpackages.
>
> The **runtime layer** — the scope tree, merge gate, settlement and recovery, the sandbox backends, the substrate/materialize layer, the task runtime, and the supervision rules engine — lives in [`github.com/buchenberg/shepherd-runtime-go`](https://github.com/buchenberg/shepherd-runtime-go), which composes this module. See that README for branching, workspace capture/restore, supervision, and task execution.

## What It Is

The kernel records agent actions as durable, inspectable traces. Every tool call, every file mutation, every agent decision becomes a record in a causal DAG with content-addressed identity.

Branching, workspace capture/restore, and supervision are built *on top of* the store by the runtime module, not here.

## Install

```bash
go get github.com/buchenberg/goatherd-kernel
```

## What You Can Do With It

### Record agent execution traces

Every tool call, every model response, every file mutation becomes a durable, content-addressed record in a causal DAG:

```go
store, _ := trace.NewSQLiteTraceStore("trace.sqlite")
ctx := context.Background()
defer store.Close(context.Background())

// Record a tool call intent
receipt, _ := store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
    AppendIntentID: "session:tool:1",
    Groups: []trace.AppendGroup{{
        TraceOwnerID: "agent:main",
        FactDrafts: []trace.RecordDraft{{
            Mode:      trace.Declaration,
            SchemaRef: "app.tool.bash.v1",
            Payload:   map[string]any{"cmd": "go test ./..."},
        }},
    }},
})

// Record the outcome
store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
    AppendIntentID: "session:result:1",
    Groups: []trace.AppendGroup{{
        TraceOwnerID:  "agent:main",
        CausalParents: receipt.FactIDs,
        FactDrafts: []trace.RecordDraft{{
            Mode:      trace.Capture,
            SchemaRef: "app.tool.bash.v1.applied",
            Payload:   map[string]any{"success": true, "duration": "2.3s"},
        }},
    }},
})
```

### Observe agent actions in real time

Subscribe to the effect bus to receive events as they happen — no polling, no SQLite reads:

```go
bus := trace.NewEffectBus(64)
store.WithBus(bus)

ch := bus.Subscribe("my-watcher")

// Events arrive as records are appended
event := <-ch
fmt.Println(event.KindLabel)  // "bash"
fmt.Println(event.Payload)    // map[cmd:go test ./...]
```

### Inspect execution history

Read back traces by owner, causal closure, or frontier checkpoint:

```go
// Everything an agent did
slice, _ := store.ReadOwnerPrefix(ctx, trace.TrustedReadContext, "agent:main", 999, trace.ModeBoth)

// Causal closure from a specific record
slice, _ = store.ReadCausalClosure(ctx, trace.TrustedReadContext, []string{"sha256:abc"}, trace.ModeBoth, "include_external_anchors")

// Point-in-time checkpoint
frontier, _ := store.PublishFrontier(ctx, trace.TrustedAppendContext, trace.FrontierSpec{...})
checkpoint, _ := store.ResolveFrontier(ctx, trace.TrustedReadContext, frontier.FrontierID, trace.ModeBoth)
```

## Trace UI service

The `traceui` package serves a read-only HTTP projection of a trace store —
the server half of the trace visualizer; the browser client lives in its own
repository and consumes this contract:

```go
store, _ := trace.NewSQLiteTraceStore("trace.sqlite")
bus := trace.NewEffectBus(64)
store.WithBus(bus)
svc, _ := traceui.New(traceui.Config{Store: store, Bus: bus})
http.Handle("/", svc.Handler()) // mount anywhere; embeds in the writer for live events
```

| Endpoint | Behavior |
|---|---|
| `GET /api/health` | liveness, bus presence, fact count |
| `GET /api/records?path=&through=&mode=` | records under an owner path subtree — the path and its `:`-delimited descendants (empty spans all owners) — with payloads and causal edges; a record retained on several paths appears once per path with that path's own owner and ordinal. `through` is an inclusive per-owner ordinal cutoff; mode filter optional. Arrays are never null |
| `GET /api/events` | server-sent events, one `effect` per appended record — requires the bus (embedded mode); without one it answers 503 and the client polls `/api/records` |

Records carry id, digest, schema, mode, kind, owner, per-owner ordinal,
context and witness refs, causal parents, and — under the trusted read the
service performs — the retained payload. Witness records appear on their own
`kernel:witness` owner path. The bus is non-blocking: a stalled client drops
events and repairs by re-reading `/api/records`; the content-addressed ids
make the merge trivial.

`cmd/traceui` is a standalone read-only viewer for an existing trace file
(`traceui -db trace.sqlite -addr 127.0.0.1:8321`) — no bus, so clients poll.
It opens the database read-only (`trace.OpenSQLiteTraceStoreReadOnly`): no
journal-mode pragma, no schema creation, and the existing schema is verified,
so a wrong path fails loudly instead of serving an empty or modified trace.

## Architecture

The kernel is small and acyclic:

```
trace  (module root)        the record model, canonical JSON, the SQLite store,
                            the effect bus, the projections
  ├── traceui               the read-only HTTP projection
  └── testkit               shared test support (vector schema + golden provenance)
```

| Package | Contents |
|---|---|
| `trace` (module root) | the record model, canonical JSON, the SQLite trace store, the effect bus, execution/history/relation projections, the checkpoint-blob and materialization-ledger plumbing |
| `traceui` | the read-only HTTP projection of a trace store |
| `testkit` | test support shared with the runtime module — the execution-vector schema and the golden-provenance tests |

The runtime packages — the root `shepherd` hub (scope tree, merge gate,
settlement, recovery), `sandbox`/`sandbox/git`, `substrate`, `materialize`,
`task`, and `supervisor` — live in
[`github.com/buchenberg/shepherd-runtime-go`](https://github.com/buchenberg/shepherd-runtime-go).

## Core Concepts

### Records

Every trace entry is a **record** with content-addressed identity — `record_id == SHA-256(canonical_input)`. Two records with identical content produce the same ID.

Records have a **mode**:
- `declaration` — an intention (what the agent proposed)
- `capture` — an observation (what actually happened)

### Witnesses

Records cite a **witness** that describes the authority and environment under which they were accepted. Witnesses chain to a deterministic root witness (genesis record).

### Causal Edges

Records declare their causal parents via `caused_by`. Parent order is part of record identity. This creates a DAG of provenance.

### Frontiers

A **frontier** is an immutable bookmark pointing to a specific record on a specific owner path. Enables point-in-time reads and undo.

### Slices

A **slice** is a graph-shaped read result — the selected records, their causal edges, witness support, and anchors for out-of-scope evidence.

### Effect Bus

A transient pub/sub layer that fans out `EffectEvent` messages to subscribers. Non-blocking — slow subscribers get their oldest event dropped, never block the writer.

### Workspace State

A **`WorkspaceState`** is a backend-neutral snapshot of a workspace — the replacement for the git-specific stash/HEAD pair:

```go
type WorkspaceState struct {
    Backend  string         // "git", "containerd"
    Revision string         // git HEAD SHA, or a snapshot key
    Data     map[string]any // backend-specific, opaque to the kernel
}
```

`Digest()` returns the kernel canonical digest of the state (`sha256:…`), used in trace records and for drift detection. Two states with identical content digest identically regardless of map iteration order, because canonicalization sorts keys at every level.

Unlike a `Checkpoint`, a `WorkspaceState` has no lifecycle: it is reusable and stays valid, so one fork point can seed many speculative branches. (The `Sandbox` interface that produces and consumes workspace states — and the `Checkpoint` type — live in the runtime module.)

## API

### Store

```go
store, err := trace.NewSQLiteTraceStore(path)  // ":memory:" for in-memory
defer store.Close(context.Background())
store.WithBus(bus)  // optional: attach effect bus

receipt, err := store.Append(ctx, trace.TrustedAppendContext, batch)  // Append records
slice, err := store.ReadOwnerPrefix(ctx, trace.TrustedReadContext, owner, through, modeFilter)
slice, err := store.ReadCausalClosure(ctx, trace.TrustedReadContext, roots, modeFilter, closurePolicy)
frontier, err := store.PublishFrontier(ctx, trace.TrustedAppendContext, spec)  // Immutable checkpoint
slice, err := store.ResolveFrontier(ctx, trace.TrustedReadContext, frontierId, modeFilter)
ids, err := store.PreviewRecordIDs(ctx, trace.TrustedAppendContext, batch)  // Dry-run append

// Read-only open (the viewer's opener): never writes, never creates schema.
ro, err := trace.OpenSQLiteTraceStoreReadOnly(path)
```

Every store call takes a Go `context.Context` first — cancellation and
deadlines reach the SQL layer — with the trace-authority context
(`TrustedAppendContext` / `TrustedReadContext`) second.

### Effect Bus

```go
bus := trace.NewEffectBus(bufferSize)  // 0 = default (64)
defer bus.Close()

ch := bus.Subscribe("watcher-id")     // <-chan EffectEvent
bus.Unsubscribe("watcher-id")
bus.Publish(event)                     // non-blocking
```

### Constants

```go
trace.Capture                    // "capture"
trace.Declaration                // "declaration"
trace.ModeBoth                   // "both"
trace.ModeCapturesOnly           // "captures_only"
trace.ModeDeclarationsOnly       // "declarations_only"
trace.TrustedAppendContext       // Default trusted append context
trace.TrustedReadContext         // Default trusted read context

trace.ABIVersion                 // "shepherd.kernel.abi.v0"
trace.CanonicalVersion           // "shepherd.kernel.canonical.v2"

trace.SchemaExecutionCreated     // "shepherd2.execution.created.v1"
trace.SchemaExecutionStarted     // "shepherd2.execution.started.v1"
trace.SchemaExecutionCompleted   // "shepherd2.execution.completed.v1"
trace.SchemaExecutionFailed      // "shepherd2.execution.failed.v1"
trace.SchemaExecutionRelation    // "shepherd2.execution_relation.created.v1"
```

The `shepherd.*` scope/checkpoint/workspace/supervisor schema constants and
`sandbox.ErrNoSandbox` / `sandbox.ErrUnsupported` live in the runtime module.

## Executions

Executions give runs identities: a create/start/complete/fail lifecycle folded
from retained facts, with parent/child relations and an effective-history
projection. Every id is derived exactly as the Python reference derives it —
`ExecutionIDFor` and the full run sequence are pinned by Python-generated
vectors — so a Go-written trace is readable by `shepherd2` and vice versa.

```go
history, err := trace.ProjectEffectiveHistoryFromStore(ctx, store, reader, cutoff)
// root execution + active children (spawned/adopted; abandoned drops) + published facts
```

Spawns and adopts are recorded as parent-owned relation facts; abandoning a
child writes an abandoned relation under the same relation id, so the
effective-history fold drops it. The whole tree projects from one terminal
frontier, and re-opening the store file re-projects it identically.

The synchronous task facade over these projections — `task.NewRegistry`,
`StartTask`/`StartTaskSync`, and `TaskControl` — lives in the runtime module.

## Golden Vectors

The `testdata/kernel_abi_v0.json` file contains deterministic test vectors shared across implementations. If your digest of the root witness body matches `sha256:28aa527e...`, your implementation is compatible. `testkit/golden_provenance_test.go` pins the SHA-256 of every vector file in this module.

## Tests

```bash
go test ./...          # the full suite
go test -race ./...    # the suite under the race detector
go vet ./...
```

The kernel suite covers the golden vectors (hash-pinned against the Python
reference), the store ABI (append idempotency, conflicts, witness chains,
frontier/cut resolution, authorization), and the execution/history
projections. CI runs it on Linux, macOS, and Windows with the race detector
on, plus a golden-drift check against the pinned Python reference.

The scopes, checkpoints, merge gate, settlement, supervision, sandbox, and
restart-recovery suites live with those packages in the runtime module.

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo). The kernel has exactly one direct dependency.
- Go 1.21+

## License

MIT — see [LICENSE](LICENSE), matching the upstream Python reference.
