package shepherd

// This file re-exports the runtime handles component (internal/handles)
// under the public package surface. The aliases keep every existing import
// of this package source-compatible.

import handles "github.com/buchenberg/shepherd-kernel-go/internal/handles"

type ChildHandle = handles.ChildHandle
type Registry = handles.Registry
type Run = handles.Run
type TaskControl = handles.TaskControl
type TaskFunc = handles.TaskFunc

var NewRegistry = handles.NewRegistry
var StartTask = handles.StartTask
var StartTaskSync = handles.StartTaskSync
