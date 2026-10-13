# Development Plan

The living plan for `shepherd-kernel-go`. The port of the Python reference
is complete: all five workstreams shipped through `v0.13.0`, all 25 kernel
laws are pinned, and the module is organized into its final pre-freeze
package layout. What remains is the v1.0 gate below, then the API freeze.

The completed workstreams are archived verbatim in
[`docs/history/`](history/) — the master plan with every task ticked, the
workstream plans 01–06, the parity comparison, and the historical gap and
dev plans. This document carries only what is still in motion.

## v1.0 gate

- [x] All plans 01–05 acceptance criteria checked; law-coverage map
      complete. *(The parity plan closed at v0.9.0 — see the CHANGELOG's
      v0.9.0 section and the archived master plan's §0 reconciliation —
      and `docs/law-coverage.md` maps all 25 laws with no accepted gaps.)*
- [x] Golden/store/schema vectors green in CI against the pinned upstream
      tag; the upstream version is recorded in
      [`testdata/UPSTREAM.md`](../testdata/UPSTREAM.md). *(The golden-drift
      CI job is green on every push.)*
- [x] Durability: full restart-recovery suite green on 3 OSes. *(Ungated
      suite under `go test -race ./...` in the ubuntu/macos/windows CI
      matrix, green through v0.13.0.)*
- [x] `context.Context` throughout; no wall-clock-ordered IDs. *(The
      plan-05 §6 sweep shipped in v0.9.0; record IDs are content-addressed
      digests.)*
- [x] LICENSE, CHANGELOG, README audited. *(MIT, matching upstream; the
      CHANGELOG covers through v0.13.0; the README was audited against
      the actual signatures and package graph.)*
- [x] Packaging decision (a): the git backend lives at `sandbox/git`
      within the core module, self-registering as `"git"`; `sandbox` keeps
      the contract and the registry. *(Landed in v0.13.0.)*
- [x] Packaging decision (b): the task runtime (`task`) and the rules
      engine (`supervisor`) are their own packages; the root package keeps
      the hub — scope tree, merge gate, settlement, recovery — because
      `ScopeManager` owns those verbs and `RecoverScopes` rebuilds its
      internals. *(Landed in v0.13.0.)*
- [ ] yaah integrated on ≥v0.9.0 for one full release cycle without
      workarounds. *(The clock starts once yaah migrates to the v0.13.0
      import paths — the CHANGELOG's v0.13.0 section is the migration
      table.)*
- [ ] Decision: API freeze of Ring-0-adjacent surface (store, canonical,
      schemas, handles) as the v1 contract; `Sandbox`/`Supervisor` remain
      explicitly extensible. *(The final gate item — after the yaah cycle
      passes clean.)*

## Backlog

Revisit triggers, not commitments (carried from the archived parity plan
§5):

- **Landlock exec-wrapper** for git-sandbox Exec (Linux): a `JailedExec`
  option on `SandboxSpec` would bring OS enforcement to the portable
  backend. Trigger: a consumer runs untrusted code without containerd.
- **Reversibility metadata on schema refs** (a registry mapping schema →
  AUTO/COMPENSABLE/NONE, used to order settlement and flag irreversible
  captures). Trigger: substrates expose more than one backend class with
  compensation needs.
- **Trace inspection CLI** (`inspect`/`log`/`diff` over a trace file).
  Trigger: debugging a field deployment without yaah. (The `traceui` HTTP
  service is the server half of this story.)
- **ATIF/trajectory export** of Go traces. Trigger: external eval tooling
  needs it.
- **macOS clonefile carrier** as an isolation `Sandbox` backend.
  Trigger: a consumer needs isolated workspaces on macOS.

## Living documents

| Document | Contents |
|---|---|
| `docs/law-coverage.md` | the 25 kernel laws and their Go tests — the maintenance rule lives there |
| `docs/DEVELOPMENT.md` | this document — the gate and the backlog |
| `testdata/UPSTREAM.md` | the pinned Python reference and the re-pin procedure |

## Release procedure

1. Branch `release/vNNN-tagged`, retitle the CHANGELOG's Unreleased
   section as the release, PR, merge.
2. Tag the release at the last **content** merge — not the changelog
   merge — per the convention of v0.11.0 through v0.13.0.
3. Push the tag; the `release verify (tagged module)` CI job must pass:
   no replace directives in tagged modules, every module resolvable from
   a clean consumer.
