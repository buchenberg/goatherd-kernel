package shepherd

// This file re-exports the supervision component (internal/supervision)
// under the public package surface. The aliases keep every existing import
// of this package source-compatible.

import supervision "github.com/buchenberg/shepherd-kernel-go/internal/supervision"

type Intervention = supervision.Intervention
type InterventionType = supervision.InterventionType
type SupervisionRule = supervision.SupervisionRule
type Supervisor = supervision.Supervisor

const InterventionDeny = supervision.InterventionDeny
const InterventionDiscard = supervision.InterventionDiscard
const InterventionFork = supervision.InterventionFork
const InterventionHalt = supervision.InterventionHalt
const InterventionInject = supervision.InterventionInject

var DestructiveToolGuard = supervision.DestructiveToolGuard
var DestructiveToolRule = supervision.DestructiveToolRule
var HighErrorRateRule = supervision.HighErrorRateRule
var NewSupervisor = supervision.NewSupervisor
var StuckDetectionRule = supervision.StuckDetectionRule
