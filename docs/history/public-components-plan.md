# Public components plan (target: v0.11.0)

> **Archived.** This plan is complete; its status is frozen. The living
> plan — the v1.0 gate and the backlog — is [`docs/DEVELOPMENT.md`](../DEVELOPMENT.md).


Status: executed on reorg/public-components (S1-S7); tag v0.11.0 after merge. Prerequisite: the component-extraction PR (alias stage)
merges to main first. No release tag is cut between that merge and this
plan landing — pkg.go.dev renders the latest tag, so the alias stage must
never be the published face of the module.

## Goal

Replace the root package's alias facade with published component packages so
that pkg.go.dev renders full documentation (methods, fields, doc comments)
for every public symbol, while keeping the compiler-enforced component
layering the extraction established.

This is the ecosystem-standard shape for a library whose contracts are
user-implemented (the Go Cloud Kit pattern: a public interface package plus
public registered backends). It closes the one real regression of the alias
stage: `type Scope = scope.Scope` renders as a name with no documentation,
because `internal/` has no published doc page.

## Non-goals

- No behavior changes. Every step is a move, an import-path edit, or
  documentation. The 300-test suite, the pinned vectors, and the digest laws
  must stay green and unchanged throughout.
- No restructuring inside components beyond what the alias stage already did.
- No new features in the public API. Where a symbol must move packages, it
  moves verbatim.

## Target layout

```
goatherd-kernel/
├── (root, package shepherd)   composition layer: Scope, ScopeManager,
│                              Checkpoint, handles (Registry, StartTaskSync,
│                              TaskControl, ChildHandle, Run), settlement
│                              (RetainedOutput, Seal/Settle), merge gate,
│                              recovery, Supervisor — as real code, plus
│                              doc.go with the package comment
├── trace/                     record model, canonical JSON, SQLiteTraceStore,
│                              effect bus, execution/history/relation
│                              projections, checkpoint-blob + ledger plumbing
├── sandbox/                   Sandbox interface, git backends, registry
│   └── containerd/             nested module (unchanged location)
├── substrate/                 Substrate contract, registry, echo/KV/workspace
├── materialize/               the materialize orchestrator
├── internal/
│   ├── gittest/               test support (stays internal)
│   └── testvectors/           test support (stays internal)
├── docs/                      planning docs
└── testdata/                  fixtures (no change)
```

Dependency graph after the plan (all edges public, still acyclic):

```
trace  <-  sandbox  <-  substrate  <-  materialize
  ^          ^
  |          |
root(shepherd) composes trace + sandbox only
```

The root package depends on `trace` and `sandbox`, not on `substrate` or
`materialize` — users who materialize import those packages directly.

## Decisions

**D1 — The composition layer returns to the root package as real code.**
`internal/scope`, `internal/handles`, and `internal/supervision` dissolve back
into the root `shepherd` package (files keep their names: scope.go,
scope_manager.go, checkpoint.go, workspace.go, settlement.go, merge_gate.go,
recovery.go, handles.go, supervisor.go). Rationale: `Scope` is the entry
point users type; a grpc-style root keeps `shepherd.NewScope(...)` and friends
at the module path, with full godoc, and the three packages were never
independently importable anyway. The root package stays one package
(~135 KB across nine well-named files) — flat where there is no consumer
seam, packaged where there is.

**D2 — The four components publish at top-level paths.** `trace/`,
`sandbox/`, `substrate/`, `materialize/`. `sandbox/` already exists as the
parent directory of the containerd nested module; a public package at
`sandbox/` and the nested module at `sandbox/containerd` coexist legally
(one is a package of the main module, the other its own module) and read
naturally. The old `internal/*` paths disappear — nothing moves to a new
`internal/` location except the test-support packages, which stay.

**D3 — Clean break, no transitional aliases.** The root facades are deleted,
not deprecated in place. Rationale: keeping root aliases for even one release
recreates the doc void for those symbols; the project is pre-1.0 and its
CHANGELOG already declares minors breaking; the known consumer surface is
small (yaah, the containerd module). Migration is a mechanical import edit,
documented in a table (below).

**D4 — Test support stays internal.** `gittest` and `testvectors` import
`testing` and exist for this module's suites. Publishing them would be a
support commitment with no consumer need.

**D5 — Add the missing package documentation.** The alias stage exposed that
no package comment exists anywhere. Each published package gets a doc
comment (root `doc.go`, `trace/doc.go`, `sandbox/sandbox.go` header, etc.)
stating the package's role and its compatibility tier. This is a strict
improvement over main, which also lacks one.

**D6 — Vocabulary stays where the extraction put it.** `WorkspaceState` and
`Containment` remain in `trace` (they are trace-recorded data; `sandbox`
already imports them). `sandbox -> trace` becomes a public dependency edge,
which is the same direction Go Cloud Kit uses (`blob` imports its own value
types package).

**D7 — The containerd nested module follows the core release.** It imports
`shepherd.Sandbox` / `shepherd.DeltaApplier` / `shepherd.WorkspaceState`
today. After this plan those become `sandbox.Sandbox`, `sandbox.DeltaApplier`,
`trace.WorkspaceState`. The module gets an import-path update, a repin to
core v0.11.0, and tags `v0.2.0` in the same release train. Its fake-based
tests must pass; the env-gated live harness is unchanged.

## Sequencing

Each step is one commit on `reorg/public-components` (branched from main
after the alias-stage PR merges). Every step ends with: `go build ./...`,
`go vet ./...`, `gofmt -l` empty, `go test -count=1 ./...` green with the
same 300 top-level tests, and the out-of-module consumer compiling.

**S1 — Dissolve the runtime components into root.**
`git mv` the nine runtime files (plus supervisor) from `internal/scope`,
`internal/handles`, `internal/supervision` to the root; package clause back
to `shepherd`; qualify their references to component packages through the
still-present facades (no edits needed — the facades are the compatibility
layer). Move the component test files (scope/checkpoint/merge_gate/
recovery/settlement/workspace/handles/supervision tests) to root;
`testdata/` paths revert to their original root-relative form
(`testvectors.LoadExecutionVectors(t, "testdata/...")` — the root helper
already reads that form). Fold each package's `helpers_test.go` into
`testhelpers_test.go`, deduplicating against `gittest`/`testvectors`.
The root facades for scope/handles/supervision disappear in the same commit
(they alias symbols that are now local again). Gate: root `go doc -all .`
renders Scope's 21 methods again.

**S2 — Promote `trace`.**
`git mv internal/trace trace` (minus `gittest`-independent test moves — the
trace tests move with the package), re-point every importer in the module
from `internal/trace` to `trace`, delete `trace_facade.go`, and qualify root
references (`trace.RecordDraft`, `trace.NewSQLiteTraceStore`, ...). The
internal-only package-level functions (`trace.NewTraceStoreError`,
`trace.PersistCheckpointBlob`, `trace.ReadCompletedMaterialization`, ...)
become unexported again where their only caller moved into root in S1
(audit per symbol; keep exported only what crosses packages: scope at root
uses the blob and recovery helpers, so they stay exported — but now they are
a *published* API decision, see Risks R3).

**S3 — Promote `sandbox`.** Same move; delete `sandbox_facade.go`; root and
substrate re-point.

**S4 — Promote `substrate`.** Same; delete `substrate_facade.go`.

**S5 — Promote `materialize`.** Same; delete `materialize_facade.go`. After
this commit no `*_facade.go` remains and no root aliases exist.

**S6 — Documentation.** Add `doc.go` (root) and package comments for the
four components; rewrite the README quickstart and API walkthrough import
paths; update the README package-layout table and diagram; write the
CHANGELOG `v0.11.0` section including the migration table below.

**S7 — Containerd module.** In `sandbox/containerd`: update the nine files
importing the core root package to the new paths, repin
`go.mod` to `goatherd-kernel v0.11.0` (via `replace` while v0.11.0 is
unreleased, direct require after tagging), run its fake-based suite. Its
version notes join the same CHANGELOG entry.

Tag order: merge the Pattern-1 PR, tag core `v0.11.0`, then tag
`sandbox/containerd/v0.2.0` (its go.mod can only require the released
version).

## Migration table (for the CHANGELOG)

| v0.10 (root) | v0.11 |
|---|---|
| `shepherd.SQLiteTraceStore`, `NewSQLiteTraceStore`, `Append`, `Read*`, `PublishCut`, `RecordDraft`, `Record`, `AppendBatch/Group/Context/Receipt`, `ReadContext`, `EffectBus`, `NewEffectBus`, `CanonicalJSON*`, `CanonicalDigest`, `Execution*`, `Relation*`, `Project*`, `Schema*` consts, `Witness*`, `Frontier*`, `Slice`, `Visibility*`, `Mode*`, `Contain*`, `Op*`, `Trusted*Context` | `trace.` (same names) |
| `shepherd.Sandbox`, `SandboxSpec`, `SandboxCapabilities`, `ExecRequest/Result`, `NewLocalGitSandbox`, `NewWorktreeSandbox`, `GitSandbox`, `OpenSandbox`, `RegisterSandbox`, `SandboxBackends`, `DeltaApplier`, `ErrUnsupported`, `ErrNoSandbox`, `ErrUnknownSandboxBackend` | `sandbox.` (same names) |
| `shepherd.Substrate`, `SubstrateRegistry`, `NewSubstrateRegistry`, `NewEchoSubstrate`, `NewKVSubstrate`, `NewWorkspaceSubstrate`, `EchoSubstrate`, `KVSubstrate`, `WorkspaceSubstrate`, `ErrUnknownSubstrate`, `SubstrateError`, `SchemaWorkspace*` consts | `substrate.` (same names) |
| `shepherd.Materialize`, `MaterializationRequest`, `MaxOwnerOrdinal` | `materialize.` (same names) |
| `shepherd.WorkspaceState` | `trace.WorkspaceState` (used in `sandbox` signatures) |
| `shepherd.Scope`, `NewScope`, `ScopeManager`, `Checkpoint*`, `StartTaskSync*`, `Registry`, `TaskFunc`, `TaskControl`, `ChildHandle`, `Run`, `RetainedOutput`, `Settle*`, `Seal`, `MergeReviewer`, `MergeProposal`, `RecoverScopes`, `Supervisor`, `SupervisionRule`, `Intervention` | unchanged at root |

## Verification and acceptance

1. All gates green at every step (above); final run includes
   `go test -count=1 -race ./...` (CI parity) and the containerd module's
   fake-based suite.
2. Test parity: the same 300 top-level tests as the alias stage; no test
   renamed, skipped, or deleted.
3. Doc rendering is the acceptance criterion for the whole plan:
   `go doc -all .` lists the root runtime types with full method sets;
   `go doc -all ./trace`, `./sandbox`, `./substrate`, `./materialize` render
   complete documentation. No public alias remains
   (`grep -r '= trace\.\|= sandbox\.' *_facade.go` finds nothing — the
   files are gone).
4. Out-of-module consumer: the compile-check program is rewritten against
   the new import paths and passes; additionally, a legacy-path check
   asserts the old imports no longer compile (clean-break confirmation).
5. No `internal/` package other than `gittest`/`testvectors` remains.

## Risks

**R1 — Consumer churn.** Any importer of the moved symbols breaks once.
Mitigation: single breaking release (v0.11.0), migration table in the
CHANGELOG, and the alias stage already proved which symbols those are.
The alternative — deprecated root aliases for one release — re-creates the
doc void and is rejected (D3).

**R2 — `sandbox/` path dualism.** A public package whose directory also
contains a nested module is legal but unusual. If it reads badly in
practice, the fallback is renaming the public package dir (`backends/`);
decided in review of S3, not now.

**R3 — Publishing internal-only helpers.** S1 moves the recovery/checkpoint
callers of `trace.PersistCheckpointBlob` / `LoadCheckpointBlobs` /
`DeleteCheckpointBlob` / `ScanRecordsBySchema` / the materialization ledger
functions into root, so those functions cross a *public* package boundary
and become importable by anyone. Mitigation: audit in S2. Options: (a) accept
— they are coherent store operations; (b) re-home their callers' plumbing
(the checkpoint blob table, the ledger table) as methods on `SQLiteTraceStore`
with clear docs, accepting that as public API deliberately. Default: (a)
with doc comments marking them as advanced/recovery surfaces.

**R4 — Root package size.** ~135 KB of source in one package. Acceptable:
files are seam-named and the package is one conceptual layer (the
orchestrator). Splitting it again would reintroduce aliases or phantom
seams.

**R5 — Docs regressions in review noise.** S1-S5 are large mechanical
diffs; review fatigue is a real risk. Mitigation: one component per commit,
pure moves, and the verification gates above run per commit.

## Rollback

Each step is a revertible commit on a branch; nothing is tagged until the
whole plan lands. If the plan stalls mid-way, main still holds the alias
stage, which is fully compatible with v0.10 consumers.

## Size estimate

Seven commits, all mechanical. The S1 test-file relocations are the only
fiddly part (testdata paths, helper dedup). Reviewable in one PR; expected
diff well under the extraction PR's.
