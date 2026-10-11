package shepherd

// This file re-exports the sandbox component (internal/sandbox) under the
// public package surface. The aliases keep every existing import of this
// package source-compatible.

import sandbox "github.com/buchenberg/shepherd-kernel-go/internal/sandbox"

type DeltaApplier = sandbox.DeltaApplier
type ExecRequest = sandbox.ExecRequest
type ExecResult = sandbox.ExecResult
type GitSandbox = sandbox.GitSandbox
type GitSandboxConfig = sandbox.GitSandboxConfig
type Sandbox = sandbox.Sandbox
type SandboxCapabilities = sandbox.SandboxCapabilities
type SandboxFactory = sandbox.SandboxFactory
type SandboxSpec = sandbox.SandboxSpec
type UnknownSandboxBackendError = sandbox.UnknownSandboxBackendError

var ErrNoSandbox = sandbox.ErrNoSandbox
var ErrUnknownSandboxBackend = sandbox.ErrUnknownSandboxBackend
var ErrUnsupported = sandbox.ErrUnsupported
var NewLocalGitSandbox = sandbox.NewLocalGitSandbox
var NewWorktreeSandbox = sandbox.NewWorktreeSandbox
var OpenSandbox = sandbox.OpenSandbox
var RegisterSandbox = sandbox.RegisterSandbox
var SandboxBackends = sandbox.SandboxBackends
