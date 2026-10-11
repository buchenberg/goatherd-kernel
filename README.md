# shepherd-kernel-go

Go implementation of the [Shepherd](https://arxiv.org/abs/2605.10913) trace kernel with real-time effect bus, execution scopes, and meta-agent supervision.

A content-addressed, append-only, causally-linked execution trace store over SQLite — plus the primitives needed to supervise agent execution in real time.

## What It Is

The kernel records agent actions as durable, inspectable, reversible traces. Every tool call, every file mutation, every agent decision becomes a record in a causal DAG with content-addressed identity.

Reversibility is real, not notional: scopes carry a pluggable **Sandbox** that captures, restores, and diffs the workspace they operate on, so a speculative branch can be rolled back or discarded outright. Three sandbox backends ship: an in-place git materializer that works on any platform, a git-worktree backend that isolates each branch in its own checkout, and a containerd backend that runs each branch in its own container (a nested module; see [docs/containerd.md](docs/containerd.md)).

This is a standalone Go module implementing `shepherd.kernel.abi.v0` — the same frozen ABI as the Python reference (`shepherd2`) and produces byte-identical digests from the same golden test vectors.

## Install

```bash
go get github.com/buchenberg/shepherd-kernel-go
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

### Fork and branch execution

Create isolated execution branches for speculative work. Fork records the branch point in the trace; merge or discard the branch later:

```go
mgr := shepherd.NewScopeManager(store)

// A root scope carries the workspace it operates on.
parent, _ := mgr.Create("agent:main", git.NewLocalGitSandbox(repoPath))
child, _ := mgr.Fork(parent.ID(), "agent:experimental", nil)

// Child runs in isolation...
// If it works:
parent.Merge(child)
// If it fails:
parent.Discard(child)  // child's records remain, but semantically abandoned
```

### Roll the workspace back

Checkpoints capture workspace state plus an opaque caller snapshot (typically
conversation history). A restore rewinds both:

```go
cp, _ := mgr.CreateCheckpoint(ctx, scope.ID(), conversationSnapshot)

// ...the sub-agent makes destructive changes...

snapshot, _ := mgr.RestoreCheckpoint(ctx, cp.ID)  // workspace reverted, snapshot returned
// Checkpoints are single-use: restore marks them used and releases the snapshot.
```

Reusable (non-consuming) workspace states are available when you need to apply
the same state more than once — that is what fork-and-choose requires:

```go
forkState, _ := scope.CaptureWorkspace(ctx)   // no lifecycle, stays valid
// ...variant A...
scope.ApplyWorkspace(ctx, forkState)          // rewind to the fork point
// ...variant B...
```

Checkpoint snapshots persist in the trace database (bounded by a per-store
blob limit), so they survive the process:

### Recover after a restart

Scopes, checkpoints, and sealed outputs are rebuilt from the durable trace
after a crash or restart — the trace is the registry. Sandboxes are never
invented: the host's resolver decides which worktrees and containers to
re-adopt and which to orphan-clean:

```go
mgr, err := shepherd.RecoverScopes(store, shepherd.RecoverOptions{
    // backend and path come from the fork record the trace durably holds;
    // both are empty for roots, and shared children are never resolved —
    // they inherit the parent's re-adopted sandbox.
    Resolver: func(scopeID, backend, path string) (sandbox.Sandbox, error) {
        return mySandboxFor(scopeID, backend, path) // nil = recover this scope causally only
    },
})
// The tree, terminal states, fork baselines, checkpoints, and retained
// outputs are back. A checkpoint whose recorded revision no longer matches
// the re-adopted workspace is CheckpointInvalid — never silently restorable.
cp := mgr.LatestCheckpoint(scopeID)
```

Recovered children still propose merges against their recovered fork
baselines, and unconsumed outputs settle exactly as they would have
before the restart. Long-lived hosts can call `mgr.PruneTerminal()` to
unregister merged/discarded scopes when their settled state is no longer
needed in-process.

### Isolate a branch in its own worktree

The git backend has two modes. In-place operates on your repository directly.
Worktree allocates a detached `git worktree` per scope, so a speculative branch
cannot touch the parent tree and `Discard` becomes a physical rollback:

```go
sb := git.NewWorktreeSandbox(repoPath, worktreePath)
sb.Create(ctx, sandbox.SandboxSpec{})
defer sb.Destroy(ctx)

// Scope-scoped ownership: the sandbox is destroyed by Discard/Merge/Halt.
child, _ := mgr.ForkIsolated(parent.ID(), "agent:experimental", nil, sb)
```

Forking is `Create` then `Apply(forkState)`: the worktree lands on the fork HEAD,
then the fork-point state is replayed so the variant sees the parent's exact
(uncommitted) tree. Captured state survives teardown because worktrees share the
repository's object store.

### Supervise sub-agents

Define rules that watch for dangerous patterns and intervene automatically:

```go
sup := supervisor.NewSupervisor(mgr, bus)
sup.AddRule(supervisor.DestructiveToolRule())          // advisory warning on destructive calls
sup.AddRule(supervisor.DestructiveToolGuard())         // deny blockable destructive calls at check time
sup.AddRule(supervisor.HighErrorRateRule(0.5, 10))     // alert at 50% errors
sup.AddRule(supervisor.StuckDetectionRule(3))          // detect loops
sup.Start("orchestrator")

for iv := range sup.Interventions() {
    switch iv.Type {
    case supervisor.InterventionInject:
        sup.Inject(iv.ScopeID, "try a different approach")
    case supervisor.InterventionHalt:
        sup.Halt(iv.ScopeID)
    }
}
```

### Review before merge / settle outputs

Nothing from a child reaches the parent without passing a review gate.
`CommitMerge` proposes the child's diff (against the baseline captured at
fork time), records it, runs your reviewer, and either merges or discards —
a denial returns `*SupervisorDeniedError` and never touches the parent:

```go
// Commit-time review: shared-sandbox child, rollback via checkpoint restore.
err := mgr.CommitMerge(ctx, child.ID(), shepherd.DraftsOnlyReviewer("drafts"))
var denied *shepherd.SupervisorDeniedError
if errors.As(err, &denied) {
    _, _ = mgr.RestoreCheckpoint(ctx, cp.ID) // roll the shared workspace back
    // retry with the denial reason injected as guidance
}
```

For isolated children (worktrees), physical propagation belongs to
settlement: seal each output, compare the frozen `Changes`, then settle
exactly one — best-of-n in a dozen lines:

```go
var outs []*shepherd.RetainedOutput
for i := 0; i < n; i++ {
    child, _ := mgr.ForkIsolated(parent.ID(), fmt.Sprintf("try-%d", i), nil, worktreeFor(repo, i))
    // ... the attempt does its work in the worktree ...
    out, _ := mgr.Seal(ctx, child.ID())
    outs = append(outs, out) // out.Changes and out.Diff are frozen at seal time
}

if err := mgr.Settle(ctx, outs[winner].ID, shepherd.SettleSelect); err != nil { // parent unmoved: fast-forward
    // parent moved since the fork? three-way instead:
    //   disjoint paths  -> mgr.Settle(ctx, outs[winner].ID, shepherd.SettleApply)
    //   path overlap    -> ErrApplyConflict, output stays unconsumed
}
for _, out := range losers {
    _ = mgr.Settle(ctx, out.ID, shepherd.SettleDiscard)
}
```

Every settlement is consume-once (`ErrOutputConsumed` on a second settle);
`selected` requires the parent to be exactly at the fork baseline, `applied`
requires path-disjoint parent changes, `released` keeps the sealed state
addressable without integrating it, and `discarded` tears the child down
keeping the sealed state as a record. Rule engines join the same gate via
`Supervisor.ReviewMerge`, which presents proposals to the rules as
`merge:proposed` events.

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

### Materialize recorded intents

Declarations are commitments: records that say what an agent *intended* to do.
Materialization is the other half of that rhythm — applying recorded
declarations through a typed substrate and appending the receipts as
captures, so the trace describes both sides of the transition:

```go
registry := substrate.NewSubstrateRegistry()
registry.Register(substrate.NewWorkspaceSubstrate(sandbox))  // or a KV, echo, custom substrate

receipt, err := materialize.Materialize(ctx, store,
    appendCtx.ToOperationContext(trace.OpMaterialize),
    materialize.MaterializationRequest{
        AppendIntentID:            "run:42:materialize",
        TargetTraceOwnerID:        "agent:main",
        TargetRecordIDs:           declarationIDs, // workspace.file.write.v1 records
        TargetThroughOwnerOrdinal: materialize.MaxOwnerOrdinal,
    },
    registry,
)
// receipt.Outcome, receipt.ProducedRecordIDs — applied captures, caused by
// their declarations, witness-stamped with the substrate that applied them.
```

The transition is idempotent once an intent has *completed*: a same-intent
retry replays the stored receipt from the completed-intent ledger without
redispatching the substrate, and a same-intent request with different content
is a conflict, not a silent second write. Dispatch itself is at-least-once:
in the window between the substrate call and the ledger write — including a
crash there — the substrate runs again. Deterministic substrates (the
reference Echo and KV substrates) converge to the identical receipt in that
window; a non-deterministic one fails loudly as an intent conflict rather
than double-writing. The substrate is selected by the declarations' own
witnesses — the `substrate_ref` stamped when they were appended — never
guessed from schemas.

`WorkspaceSubstrate` bridges the kernel's `Sandbox` interface, so recorded
file-write and exec intents reach a real workspace (git, worktree, or
containerd) through the same declare→capture rhythm, gated on the backend's
capabilities: the git backend honestly reports `clean_failure` instead of
half-applying. Outcome honesty is the contract: `clean_failure` is claimed
only when nothing was attempted — a failure *after* a sandbox call was made
is reported as `split_state` even when no capture landed, because backends
write files in chunks and the world may have changed without a trace. The
reference KV substrate (`substrate.NewKVSubstrate`) keeps a world-side SQLite
store separate from the trace, and the echo substrate is the test fixture.

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

### Package layout

The module is organized by component, and the component packages are
public: import them directly. The root `shepherd` package is the runtime
layer — the scope tree, merge gate, settlement, and recovery — and the
task handles and supervision rules engine compose it from their own
packages. The dependency graph is acyclic, with `trace`
as the leaf:

```
trace  <-  sandbox  <-  substrate  <-  materialize
  ^          ^
  |          |
  +----------+
       |
  root (shepherd: scope, merge gate, settlement, recovery)
       +-- task (runtime handles)      +-- supervisor (rules engine)
```

| Package | Contents |
|---|---|
| `shepherd` (root) | the runtime layer — `Scope`, `ScopeManager`, checkpoints, workspace operations, the merge gate, settlement (`Seal`, `Settle`, `RetainedOutput`), recovery (`RecoverScopes`) |
| `task` | the task runtime — `StartTask`/`StartTaskSync`, `Run`, `TaskControl`, the task `Registry`, `ChildHandle` |
| `supervisor` | the rules engine — `Supervisor`, `SupervisionRule`, the built-in rules (`DestructiveToolRule`, `HighErrorRateRule`, `StuckDetectionRule`, `DestructiveToolGuard`) |
| `trace` | the record model, canonical JSON, the SQLite trace store, the effect bus, execution/history/relation projections, the checkpoint-blob and materialization-ledger plumbing |
| `sandbox` | the `Sandbox` interface, the backend registry |
| `sandbox/git` | the git backend (in-place, worktree); self-registers as "git" |
| `substrate` | the `Substrate` contract, the registry (unknown substrate refs fail the materialization), the echo/KV/workspace substrates |
| `materialize` | the materialize orchestrator (owns the append; substrates never do) |
| `internal/gittest`, `internal/testvectors`, `internal/rttest` | test support shared across packages |
| `sandbox/containerd` | the containerd backend, a nested module |

The v0.11 package move broke imports that used the root package for
component symbols (`shepherd.NewSQLiteTraceStore` -> `trace.NewSQLiteTraceStore`,
`shepherd.NewLocalGitSandbox` -> `sandbox.NewLocalGitSandbox`, and so on);
the CHANGELOG's v0.11.0 section carries the full migration table. Runtime
symbols (`shepherd.NewScope`, `shepherd.NewScopeManager`, ...) are
unchanged. (The git constructors have since moved again — to `sandbox/git`
— and the task/supervisor symbols to their own packages; the CHANGELOG's
v0.13.0 section carries those migrations.)

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

### Scopes

A **scope** represents a branch of execution. Fork creates a child scope with its own trace owner; merge propagates causal links; discard abandons the branch. Lifecycle events are recorded in the trace.

A scope also carries an optional **sandbox** — its execution substrate. A scope created without one is *pure-causal*: it participates in the trace DAG but workspace operations return `ErrNoSandbox`. `Fork` inherits the parent's sandbox without owning it; `ForkIsolated` attaches a new sandbox that the child owns, so `Discard`/`Merge`/`Halt` destroy it.

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

Unlike a `Checkpoint`, a `WorkspaceState` has no lifecycle: it is reusable and stays valid, so one fork point can seed many speculative branches.

## Sandboxes

A **Sandbox** is the substrate a scope forks, snapshots, restores, diffs, and executes in. It is the seam that keeps the kernel independent of any particular isolation technology:

```go
type Sandbox interface {
    Backend() string
    Capabilities() SandboxCapabilities
    Create(ctx context.Context, spec SandboxSpec) error
    Destroy(ctx context.Context) error
    Capture(ctx context.Context) (WorkspaceState, error)
    Apply(ctx context.Context, ws WorkspaceState) error
    Diff(ctx context.Context, ws WorkspaceState, maxLines int) (string, []string, error)
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
```

Backends need not implement everything. `Capabilities()` reports what is available, and unimplemented operations return `ErrUnsupported` rather than pretending to succeed. `Capture` and `Diff` are **non-mutating**: the caller's git index is left as it was found.

Three backend modes ship, packaged on purpose:

| Backend | Mode | Lifecycle | Isolated | Containment | Package |
|---|---|---|---|---|---|
| `git` | in-place (`git.NewLocalGitSandbox`) | No — validates only | No | `uncontained` | `sandbox/git`, in the core module |
| `git` | worktree (`git.NewWorktreeSandbox`) | Yes — real worktrees | Yes | `buffered` | `sandbox/git`, in the core module |
| `containerd` | container per scope | Yes — snapshots + leases | Yes | `contained` | `sandbox/containerd`, a nested module |

- **git** — the core module's `sandbox/git` subpackage. A materializer, not an isolation boundary; zero external dependencies; the carrier the kernel's own test suite rides on. Works on any platform.
- **containerd** — a nested module so its dependency tree stays out of the core. It runs **anywhere a containerd daemon runs: native Linux, WSL2, or a Linux server** — Linux is a runtime requirement (containerd and overlayfs are Linux), not a build-tag fact; the module cross-compiles on Windows and macOS even though the daemon does not run there. It implements `sandbox.DeltaApplier` (settlement's `SettleApply` works on containerd scopes), runs unprivileged (any user sharing the containerd socket group, since `v0.2.1`), and its live verification record is in [docs/containerd.md](docs/containerd.md).

Any type implementing the `Sandbox` interface is swappable in — the
kernel never inspects a backend beyond the opaque
`WorkspaceState.Backend` string. For config-driven selection, backends
register factories and the host resolves a configured name:

```go
// Config-driven construction: the git backend (sandbox/git) self-registers, and
// the containerd backend self-registers when its package is imported.
sb, err := sandbox.OpenSandbox("git", git.GitSandboxConfig{RepoPath: repoPath})

_, err = sandbox.OpenSandbox("podman", nil)    // -> ErrUnknownSandboxBackend, listing what IS registered
backends := sandbox.SandboxBackends()          // sorted names, for config diagnostics

// Direct constructors remain available:
inPlace := git.NewLocalGitSandbox(repoPath) // materializes on your repository. Destroy is a no-op.
worktree := git.NewWorktreeSandbox(repoPath, wt) // a detached git worktree this scope owns

err = sb.Create(ctx, sandbox.SandboxSpec{})    // OpenSandbox does NOT provision; Create is the lifecycle step
defer sb.Destroy(ctx)

ws, err := sb.Capture(ctx)
err = sb.Apply(ctx, ws)
diff, files, err := sb.Diff(ctx, ws, maxLines)

caps := sb.Capabilities()   // capability negotiation: Lifecycle/Exec/FileIO/Diff/Isolated/Containment
```

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
```

### Effect Bus

```go
bus := trace.NewEffectBus(bufferSize)  // 0 = default (64)
defer bus.Close()

ch := bus.Subscribe("watcher-id")     // <-chan EffectEvent
bus.Unsubscribe("watcher-id")
bus.Publish(event)                     // non-blocking
```

### Scope Manager

```go
mgr := shepherd.NewScopeManager(store)

scope, err := mgr.Create("owner-id", sb)                            // sb may be nil (pure-causal)
child, err := mgr.Fork("parent-scope-id", "child-owner-id", nil)    // inherits parent's sandbox
child, err := mgr.ForkIsolated("parent-scope-id", "child-owner-id", nil, sb)
err := parent.Merge(child)                                          // takes *Scope, not an ID
err := parent.Discard(child)
err := mgr.DestroyScopeSandbox(ctx, "scope-id")                     // release provisioned resources
scope, ok := mgr.Get("scope-id")
active := mgr.ActiveScopes()

cp, err := mgr.CreateCheckpoint(ctx, "scope-id", snapshot)          // single-use
snapshot, err := mgr.RestoreCheckpoint(ctx, cp.ID)
latest := mgr.LatestCheckpoint("scope-id")
mgr.PruneCheckpoints(ctx, "scope-id")                 // also deletes persisted blobs

// Merge gate (plan 04): propose, review, commit-or-discard
proposal, err := mgr.ProposeMerge(ctx, "child-scope-id", 2000)      // query; classifies create/modify/delete
err = mgr.CommitMerge(ctx, "child-scope-id", shepherd.DraftsOnlyReviewer("drafts"))
// deny -> *SupervisorDeniedError, child discarded; isolated children are refused (use Seal + Settle)

// Retained outputs: seal, then settle exactly once
out, err := mgr.Seal(ctx, "scope-id")                              // scope stays active
got, ok := mgr.Output(out.ID)
outs := mgr.OutputsForScope("scope-id")
err = mgr.Settle(ctx, out.ID, shepherd.SettleSelect)               // or SettleApply / SettleRelease / SettleDiscard

// Restart durability: rebuild the registry from the trace
mgr, err = shepherd.RecoverScopes(store, shepherd.RecoverOptions{ /* Resolver: re-adopt sandboxes */ })
pruned := mgr.PruneTerminal()                                      // unregister merged/discarded scopes
```

Every store call takes a Go `context.Context` first — cancellation and
deadlines reach the SQL layer — with the trace-authority context
(`TrustedAppendContext` / `TrustedReadContext`) second.

### Scope workspace and checkpoint operations

```go
scope := shepherd.NewScope(store, "owner-id").WithSandbox(sb, false)  // owns=false: do not Destroy

ws, err := scope.CaptureWorkspace(ctx)          // reusable state, no lifecycle
err = scope.ApplyWorkspace(ctx, ws)
diff, files, err := scope.DiffWorkspace(ctx, ws, 2000)

cp, err := scope.CreateCheckpoint(ctx, snapshot)
snapshot, err := scope.RestoreCheckpoint(ctx, cp)
```

### Supervisor

A rule-based engine that watches the effect bus and emits **interventions** when rules match. Built-in rules guard against destructive operations, high error rates, and stuck detection.

```go
sup := supervisor.NewSupervisor(mgr, bus)
sup.AddRule(supervisor.DestructiveToolRule())
sup.AddRule(supervisor.HighErrorRateRule(0.5, 10))
sup.AddRule(supervisor.StuckDetectionRule(3))
sup.Start("orchestrator")
defer sup.Close()

for iv := range sup.Interventions() { ... }

err := sup.Inject(scopeID, "guidance text")
err := sup.Halt(scopeID)
```

### Materialization

```go
registry := substrate.NewSubstrateRegistry()
err := registry.Register(substrate)               // Substrate interface: SubstrateRef,
                                                  // DeclarationSchemas, CaptureSchemas,
                                                  // Containment, Materialize
substrate, err := registry.Get(substrateRef)      // ErrUnknownSubstrate when missing

receipt, err := materialize.Materialize(ctx, store, opCtx, req, registry)
// opCtx.Operation must be trace.OpMaterialize
// req: AppendIntentID, TargetTraceOwnerID, TargetRecordIDs,
//      TargetThroughOwnerOrdinal (materialize.MaxOwnerOrdinal), CaptureTraceOwnerID
// receipt: Outcome (success | clean_failure | split_state), SubstrateRef,
//          TargetRecordIDs, ProducedRecordIDs, WorldSideAnchors

echo := substrate.NewEchoSubstrate("my.echo.v1", "example.write.v1")
kv, err := substrate.NewKVSubstrate("world.sqlite")   // kv.sqlite.local.v1
ws := substrate.NewWorkspaceSubstrate(sandbox)       // workspace.sandbox.v1

result, err := substrate.Materialize(ctx, records)   // substrates never append;
                                                     // dispatch owns the append
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
shepherd.SchemaScopeForked          // "shepherd.scope.forked.v1"
shepherd.SchemaScopeMerged          // "shepherd.scope.merged.v1"
shepherd.SchemaScopeDiscarded       // "shepherd.scope.discarded.v1"
shepherd.SchemaSupervisorInject     // "shepherd.supervisor.inject.v1"
shepherd.SchemaSupervisorHalt       // "shepherd.supervisor.halt.v1"
shepherd.SchemaCheckpointCreated    // "shepherd.checkpoint.created.v2"
shepherd.SchemaCheckpointRestored   // "shepherd.checkpoint.restored.v2"
shepherd.SchemaWorkspaceCaptured    // "shepherd.workspace.captured.v1"
shepherd.SchemaWorkspaceApplied     // "shepherd.workspace.applied.v1"

shepherd.CheckpointValid            // "valid"
shepherd.CheckpointUsed             // "used"
shepherd.CheckpointInvalid          // "invalid"

sandbox.ErrNoSandbox               // scope has no sandbox
sandbox.ErrUnsupported             // backend does not implement the capability
```

### Trace schemas

The checkpoint and workspace schemas embed a backend-neutral `revision` and
`state_digest` rather than git-specific SHAs, so a trace reads the same whether
the workspace was a git tree, a worktree, or a container snapshot:

| Schema | Mode | Payload |
|---|---|---|
| `shepherd.checkpoint.created.v2` | declaration | `checkpoint_id, backend, revision, state_digest, has_snapshot` |
| `shepherd.checkpoint.restored.v2` | capture | `checkpoint_id, backend, revision, state_digest` |
| `shepherd.workspace.captured.v1` | declaration | `backend, revision, state_digest` |
| `shepherd.workspace.applied.v1` | capture | `backend, revision, state_digest` |

These are kernel extensions, not part of `shepherd.kernel.abi.v0`, so they do not
affect the golden vectors. Trace writes are advisory: a failed append never fails
the workspace operation, because the rollback guarantee does not depend on the
audit record.

## Executions & Handles

Executions give runs identities: a create/complete/fail lifecycle folded from
retained facts, parent/child relations, and a synchronous task facade over the
store. Every id is derived exactly as the Python reference derives it —
`ExecutionIDFor` and the full run sequence are pinned by Python-generated
vectors — so a Go-written trace is readable by `shepherd2` and vice versa.

```go
reg := task.NewRegistry()
reg.Register("MyTask", func(c *task.TaskControl) (map[string]any, error) {
    c.Publish(ctx, "note", map[string]any{"msg": "working"})
    child, err := c.Spawn(ctx, "LeafTask", inputs, reg)
    if err != nil { return nil, err }
    exec, err := c.AwaitTerminal(ctx, child)
    return map[string]any{"child": exec.ExecutionID}, nil
})

run, err := task.StartTaskSync(ctx, store, reg, "MyTask", "run:fixed-id", inputs)
exec, err := run.Wait(ctx)      // terminal projection: succeeded / failed

// or async: StartTask returns a live handle; Wait blocks until terminal
run, err = task.StartTask(ctx, store, reg, "MyTask", "", inputs)

history, err := trace.ProjectEffectiveHistoryFromStore(ctx, store, reader, cutoff)
// root execution + active children (spawned/adopted; abandoned drops) + published facts
```

Spawns and adopts are recorded as parent-owned relation facts; abandoning a
child writes an abandoned relation under the same relation id, so the
effective-history fold drops it. The whole tree projects from one terminal
frontier, and re-opening the store file re-projects it identically.

Godoc: https://pkg.go.dev/github.com/buchenberg/shepherd-kernel-go

## Golden Vectors

The `testdata/kernel_abi_v0.json` file contains deterministic test vectors shared across implementations. If your digest of the root witness body matches `sha256:28aa527e...`, your implementation is compatible.

## Tests

```bash
go test ./...          # the full suite; -race is on by default in CI
go test -race ./...    # the suite under the race detector
go vet ./...
```

The suite covers the golden vectors (hash-pinned against the Python
reference), the store ABI (append idempotency, conflicts, witness chains,
frontier/cut resolution, authorization), scopes and checkpoints, the merge
gate and settlement recipes, supervision, both sandbox backends (the
containerd live suite is env-gated on a daemon), and the restart-recovery
round trips. CI runs it on Linux, macOS, and Windows with the race
detector on, plus a golden-drift check against the pinned Python
reference.

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo). The core module has exactly one direct dependency.
- Go 1.21+

The `sandbox/containerd/` adapter is a nested module with its own `go.mod`, so its (much larger) dependency graph never enters the core module. It is excluded from `go build ./...` and `go test ./...` here; build it from its own directory.

## License

MIT — see [LICENSE](LICENSE), matching the upstream Python reference. The
`sandbox/containerd` nested module carries the same license under its own
path.
