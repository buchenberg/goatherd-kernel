// Package testvectors holds the shared schema of the Python-generated
// execution vector fixtures. Both the trace component's tests and the
// runtime layer's tests decode the same fixture file; keeping the schema in
// one package prevents the two suites from drifting apart.
package testvectors

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// ExecutionIDVector pins the execution id derivation.
type ExecutionIDVector struct {
	AppendIntentID string `json:"append_intent_id"`
	LocalRef       string `json:"local_ref"`
	ExecutionID    string `json:"execution_id"`
}

// RelationIDVector pins the relation id derivation.
type RelationIDVector struct {
	AppendIntentID string `json:"append_intent_id"`
	LocalRef       string `json:"local_ref"`
	RelationID     string `json:"relation_id"`
}

// PublishedFactVector pins the published fact shape.
type PublishedFactVector struct {
	SchemaRef string         `json:"schema_ref"`
	KindLabel string         `json:"kind_label"`
	Mode      string         `json:"mode"`
	Payload   map[string]any `json:"payload"`
}

// ExecutionVectorFact is one owner-path fact in a vector.
type ExecutionVectorFact struct {
	FactID    string `json:"fact_id"`
	SchemaRef string `json:"schema_ref"`
	Mode      string `json:"mode"`
	KindLabel string `json:"kind_label"`
}

// ExecutionVectorCutoff pins the terminal cutoff shape.
type ExecutionVectorCutoff struct {
	FrontierID          string `json:"frontier_id"`
	TargetTraceOwnerID  string `json:"target_trace_owner_id"`
	ThroughFactID       string `json:"through_fact_id"`
	ThroughOwnerOrdinal int    `json:"through_owner_ordinal"`
	PublisherOwnerID    string `json:"publisher_trace_owner_id"`
	CreatedByFactID     string `json:"created_by_fact_id"`
}

// ExecutionVectorStep is one replayed step.
type ExecutionVectorStep struct {
	Kind           string         `json:"kind"`
	AppendIntentID string         `json:"append_intent_id"`
	FrontierID     string         `json:"frontier_id"`
	ThroughFactID  string         `json:"through_fact_id"`
	Payload        map[string]any `json:"payload"`
	Outputs        map[string]any `json:"outputs"`
	Error          string         `json:"error"`
	FactIDs        []string       `json:"fact_ids"`
}

// ExecutionVectorRun is a full run replay vector.
type ExecutionVectorRun struct {
	RunID       string                `json:"run_id"`
	ExecutionID string                `json:"execution_id"`
	FrontierID  string                `json:"frontier_id"`
	TaskRef     string                `json:"task_ref"`
	Inputs      map[string]any        `json:"inputs"`
	Steps       []ExecutionVectorStep `json:"steps"`
	OwnerPath   []ExecutionVectorFact `json:"owner_path"`
	Cutoff      ExecutionVectorCutoff `json:"cutoff"`
}

// ExecutionVectorRelation is a relation replay vector.
type ExecutionVectorRelation struct {
	ParentRunID         string         `json:"parent_run_id"`
	ChildRunID          string         `json:"child_run_id"`
	ParentExecutionID   string         `json:"parent_execution_id"`
	ChildExecutionID    string         `json:"child_execution_id"`
	ChildFrontierID     string         `json:"child_frontier_id"`
	RelationIntent      string         `json:"relation_intent"`
	RelationID          string         `json:"relation_id"`
	ParentCreateIntent  string         `json:"parent_create_intent"`
	ParentTaskRef       string         `json:"parent_task_ref"`
	ChildCreateIntent   string         `json:"child_create_intent"`
	ChildTaskRef        string         `json:"child_task_ref"`
	ChildInputs         map[string]any `json:"child_inputs"`
	ChildCompleteIntent string         `json:"child_complete_intent"`
	ChildOutputs        map[string]any `json:"child_outputs"`
	RelationFactID      string         `json:"relation_fact_id"`
}

// HistoryVectorChild is one child in the history projection vector.
type HistoryVectorChild struct {
	ExecutionID string `json:"execution_id"`
	TaskRef     string `json:"task_ref"`
	Status      string `json:"status"`
	Kind        string `json:"kind"`
	RelationID  string `json:"relation_id"`
	FrontierID  string `json:"frontier_id"`
}

// ExecutionVectorHistory is the effective-history projection vector.
type ExecutionVectorHistory struct {
	RunID           string                `json:"run_id"`
	ExecutionID     string                `json:"execution_id"`
	FrontierID      string                `json:"frontier_id"`
	TaskRefs        map[string]string     `json:"task_refs"`
	ParentOwnerPath []ExecutionVectorFact `json:"parent_owner_path"`
	RootStatus      string                `json:"root_status"`
	RootTaskRef     string                `json:"root_task_ref"`
	Children        []HistoryVectorChild  `json:"children"`
	Published       []string              `json:"published"`
}

// ExecutionVectorDoc is the whole execution vectors fixture.
type ExecutionVectorDoc struct {
	Generator        string                  `json:"generator"`
	SourceRepo       string                  `json:"source_repo"`
	SourceCommit     string                  `json:"source_commit"`
	PythonVersion    string                  `json:"python_version"`
	ExecutionIDs     []ExecutionIDVector     `json:"execution_ids"`
	RelationIDs      []RelationIDVector      `json:"relation_ids"`
	PublishedFact    PublishedFactVector     `json:"published_fact"`
	RunSequence      ExecutionVectorRun      `json:"run_sequence"`
	FailSequence     ExecutionVectorRun      `json:"fail_sequence"`
	RelationSequence ExecutionVectorRelation `json:"relation_sequence"`
	HistorySequence  ExecutionVectorHistory  `json:"history_sequence"`
}

// LoadExecutionVectors decodes the execution vectors fixture at path, which
// is relative to the calling test's package directory.
func LoadExecutionVectors(t *testing.T, path string) ExecutionVectorDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read execution vectors: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc ExecutionVectorDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("parse execution vectors: %v", err)
	}
	return doc
}
