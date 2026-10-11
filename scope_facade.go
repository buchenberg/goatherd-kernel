package shepherd

// This file re-exports the scope-tree runtime component (internal/scope)
// under the public package surface. The aliases keep every existing import
// of this package source-compatible.

import scope "github.com/buchenberg/shepherd-kernel-go/internal/scope"

type Checkpoint = scope.Checkpoint
type CheckpointState = scope.CheckpointState
type MergeProposal = scope.MergeProposal
type MergeReviewer = scope.MergeReviewer
type OutputState = scope.OutputState
type ProposedChange = scope.ProposedChange
type RecoverOptions = scope.RecoverOptions
type RetainedOutput = scope.RetainedOutput
type Scope = scope.Scope
type ScopeManager = scope.ScopeManager
type ScopeState = scope.ScopeState
type SettlementAction = scope.SettlementAction
type SupervisorDeniedError = scope.SupervisorDeniedError

const ChangeCreate = scope.ChangeCreate
const ChangeDelete = scope.ChangeDelete
const ChangeModify = scope.ChangeModify
const CheckpointInvalid = scope.CheckpointInvalid
const CheckpointUsed = scope.CheckpointUsed
const CheckpointValid = scope.CheckpointValid
const DefaultProposalDiffLines = scope.DefaultProposalDiffLines
const OutputApplied = scope.OutputApplied
const OutputDiscarded = scope.OutputDiscarded
const OutputInvalid = scope.OutputInvalid
const OutputReleased = scope.OutputReleased
const OutputSelected = scope.OutputSelected
const OutputUnconsumed = scope.OutputUnconsumed
const SchemaCheckpointCreated = scope.SchemaCheckpointCreated
const SchemaCheckpointRestored = scope.SchemaCheckpointRestored
const SchemaMergeProposed = scope.SchemaMergeProposed
const SchemaRunOutputSealed = scope.SchemaRunOutputSealed
const SchemaRunOutputSettled = scope.SchemaRunOutputSettled
const SchemaScopeDiscarded = scope.SchemaScopeDiscarded
const SchemaScopeForked = scope.SchemaScopeForked
const SchemaScopeMerged = scope.SchemaScopeMerged
const SchemaSupervisorDecision = scope.SchemaSupervisorDecision
const SchemaSupervisorHalt = scope.SchemaSupervisorHalt
const SchemaSupervisorInject = scope.SchemaSupervisorInject
const SchemaWorkspaceApplied = scope.SchemaWorkspaceApplied
const SchemaWorkspaceCaptured = scope.SchemaWorkspaceCaptured
const ScopeActive = scope.ScopeActive
const ScopeDiscarded = scope.ScopeDiscarded
const ScopeMerged = scope.ScopeMerged
const SettleApply = scope.SettleApply
const SettleDiscard = scope.SettleDiscard
const SettleRelease = scope.SettleRelease
const SettleSelect = scope.SettleSelect

var DestructivePathReviewer = scope.DestructivePathReviewer
var DraftsOnlyReviewer = scope.DraftsOnlyReviewer
var ErrApplyConflict = scope.ErrApplyConflict
var ErrOutputConsumed = scope.ErrOutputConsumed
var NewScope = scope.NewScope
var NewScopeManager = scope.NewScopeManager
var RecoverScopes = scope.RecoverScopes
