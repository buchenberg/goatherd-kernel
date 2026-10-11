# The containerd sandbox backend

The containerd backend (`sandbox/containerd`, a nested Go module) runs each
scope's workspace in its own container: an overlay-snapshotter image, a
namespace, and a real task lifecycle. It is the isolation backend the git
backends are not — `Isolated: true`, with `ContainContained` containment
(namespaces plus a separate rootfs, but not a VM boundary: a kernel
exploit escapes it, so it is not `ContainFull`) —
and it is the backend `SettleApply` (settlement's three-way path) works on
out of the box through its `DeltaApplier` implementation.

## Packaging and requirements

- **A nested module** (`sandbox/containerd/go.mod`) so its dependency tree
  stays out of the core module. It carries its own MIT `LICENSE` (since
  `v0.2.3`) and requires the core module by tag.
- **Linux at runtime, not at build time**: containerd and overlayfs are
  Linux, so the backend needs a running containerd daemon — native Linux,
  WSL2, or a Linux server. The module itself cross-compiles on Windows and
  macOS; the daemon does not run there.
- **Self-registering**: importing the package registers it with
  `sandbox.OpenSandbox` under `"containerd"`, so config-driven selection
  needs no host wiring beyond the import.

```go
sb, err := sandbox.OpenSandbox("containerd", containerd.Config{Image: "example/dev:latest"})
```

## Verification record

The backend's correctness is pinned twice: unit tests run against
containerd's real snapshotter interface in CI (`containerd adapter
(linux)`), and an env-gated live suite runs the whole lifecycle against a
real daemon. The live harness found defects the fakes could not reach —
this is the record, newest first.

### Unprivileged operation (v0.2.1, 2026-10-11)

Any user sharing only the containerd socket group can start sandboxes. The
init spec is built from the image config blob read through the daemon's
content store (env, working directory, numeric `uid[:gid]`), never from the
client process's filesystem: the previous `oci.WithImageConfig` path
resolved the image user by reading the unpacked rootfs, which the daemon's
0700-root snapshot directories make impossible for anyone but root.
Verified live: an unprivileged client passed `Create` and the contract exec
where it previously failed with EACCES, and a full end-to-end session ran
with in-sandbox tool exec (`pwd` = `/workspace` inside the container).

### Delta merges (v0.1.5+)

The backend implements `sandbox.DeltaApplier`, so settlement's apply verb
(`SettleApply`) works on containerd scopes: a sealed output merges onto a
parent whose workspace moved past the fork, without resetting it. `Apply`
records the applied state's git HEAD as the sandbox's delta base and every
`Capture` carries a git binary patch from that base to the captured tree;
`ApplyDelta` merges at the tree level (`git merge-tree --write-tree`) in the
object database, so a conflicting delta refuses with the working tree
untouched — no markers, no partial application — and a clean one lands as
unstaged changes that leave the caller's staged work alone. Pinned live by
`TestLive_ApplyDeltaMergesOntoMovedParent` and
`TestLive_ApplyDeltaConflictRefusesUntouched`.

### State durability (v0.1.4)

A first run against a *cold* daemon (fresh namespace, no image) found that
`Destroy` deleted the committed layers backing captured states, so `Apply`
after `Destroy` failed every time. States are now durable by construction:
every `Capture` pins its committed layer under a namespace-scoped
`shepherd-states` lease that outlives any sandbox, `Destroy` removes only
the sandbox's private active layer, and an optional `StateReleaser`
interface (`ReleaseState`) is the deliberate reclaim path — disk comes
back when the caller says so, not when a sandbox happens to die. The
cold-namespace live suite that found it — including the
destroy-keeps-states acceptance sequence — is 9/9 green.

### The first soak (v0.1.3)

The first live runs against a real containerd v2.3.5 found ten defects the
fakes could not reach — exec never started its process, the rootfs snapshot
key was the manifest digest rather than the layer chainID, teardown
orphaned exec'd processes, `Diff` mutated the caller's git index, and (the
tenth) the adapter held **no containerd lease**, so the daemon's garbage
collector could reclaim a just-committed snapshot before the successor was
prepared. The sandbox now holds a labelled lease for its lifetime, and 12
consecutive live runs are green.

## Running the live suite

The harness (`sandbox/containerd/live_test.go`, build-tagged
`linux && !nolive`) skips unless `SHEPHERD_CONTAINERD_ADDR` points at a
daemon socket, so `go test ./...` stays green everywhere else:

```bash
sudo env SHEPHERD_CONTAINERD_ADDR=/run/containerd/containerd.sock \
  go test -count=1 -v -run TestLive ./...
```

A stock image and the overlayfs snapshotter suffice. The `Config.FIFODir`
knob exists precisely so a non-root WSL2 client can drive the daemon; the
live tests set it to a temp directory.

## Honesty notes

- The backend reports its capabilities through the same
  `SandboxCapabilities` contract as the git backends; unsupported
  operations return `ErrUnsupported` rather than pretending.
- Outcome honesty in materialization is the contract:
  `clean_failure` is claimed only when nothing was attempted — a failure
  *after* a sandbox call was made is reported as `split_state` even when no
  capture landed, because backends write files in chunks and the world may
  have changed without a trace.
- The `Sandbox` interface is a materialization seam, not a containment
  boundary; the containerd backend provides the Linux enforcement story,
  but "runs in a container" is not a security claim on its own.
