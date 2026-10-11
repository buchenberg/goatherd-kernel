package shepherd

// This file re-exports the materialize orchestrator (internal/materialize)
// under the public package surface. The aliases keep every existing import
// of this package source-compatible.

import materialize "github.com/buchenberg/shepherd-kernel-go/internal/materialize"

type MaterializationRequest = materialize.MaterializationRequest

const MaxOwnerOrdinal = materialize.MaxOwnerOrdinal

var Materialize = materialize.Materialize
