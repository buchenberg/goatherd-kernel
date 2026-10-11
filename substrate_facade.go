package shepherd

// This file re-exports the substrate component (internal/substrate) under
// the public package surface. The aliases keep every existing import of
// this package source-compatible.

import substrate "github.com/buchenberg/shepherd-kernel-go/internal/substrate"

type EchoSubstrate = substrate.EchoSubstrate
type KVSubstrate = substrate.KVSubstrate
type Substrate = substrate.Substrate
type SubstrateError = substrate.SubstrateError
type SubstrateRegistry = substrate.SubstrateRegistry
type UnknownSubstrateError = substrate.UnknownSubstrateError
type WorkspaceSubstrate = substrate.WorkspaceSubstrate

const KVPutCaptureSchema = substrate.KVPutCaptureSchema
const KVPutDeclarationSchema = substrate.KVPutDeclarationSchema
const KVSQLiteSubstrateRef = substrate.KVSQLiteSubstrateRef
const SchemaWorkspaceExec = substrate.SchemaWorkspaceExec
const SchemaWorkspaceExecApplied = substrate.SchemaWorkspaceExecApplied
const SchemaWorkspaceFileWrite = substrate.SchemaWorkspaceFileWrite
const SchemaWorkspaceFileWriteApplied = substrate.SchemaWorkspaceFileWriteApplied
const WorkspaceSubstrateRef = substrate.WorkspaceSubstrateRef

var ErrUnknownSubstrate = substrate.ErrUnknownSubstrate
var NewEchoSubstrate = substrate.NewEchoSubstrate
var NewKVSubstrate = substrate.NewKVSubstrate
var NewSubstrateRegistry = substrate.NewSubstrateRegistry
var NewWorkspaceSubstrate = substrate.NewWorkspaceSubstrate
