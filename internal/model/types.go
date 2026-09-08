// Package model holds the shared data types used across the client, the
// relay and the providers: captured shell events, batches, drafts and
// document locations.
package model

import (
	"encoding/json"
	"time"
)

// Kind says whether an event counts toward summarization triggers.
type Kind string

const (
	// KindNoise is a command that carries no documentation value on its own
	// (ls, cd, cat, ...). It is still stored for context.
	KindNoise Kind = "noise"
	// KindMeaningful is a command that changes something or is evidence of
	// work worth documenting.
	KindMeaningful Kind = "meaningful"
)

// Event is one captured shell command after redaction and classification.
type Event struct {
	ID           int64     `json:"id"`
	SessionID    string    `json:"session_id"`
	Seq          int       `json:"seq"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	Command      string    `json:"command"`
	ExitCode     int       `json:"exit_code"`
	CWD          string    `json:"cwd"`
	GitRoot      string    `json:"git_root,omitempty"`
	GitBranch    string    `json:"git_branch,omitempty"`
	GitHead      string    `json:"git_head,omitempty"`
	Hostname     string    `json:"hostname"`
	Username     string    `json:"username"`
	Shell        string    `json:"shell"`
	Kind         Kind      `json:"kind"`
	Family       string    `json:"family"`
	Weight       int       `json:"weight"`
	Milestone    bool      `json:"milestone"`
	FilesTouched []string  `json:"files_touched,omitempty"`
	Redactions   []string  `json:"redactions,omitempty"`
	BatchID      string    `json:"batch_id,omitempty"`
}

// Duration is the wall-clock time the command took.
func (e Event) Duration() time.Duration {
	if e.End.Before(e.Start) {
		return 0
	}
	return e.End.Sub(e.Start)
}

// Session is one interactive shell.
type Session struct {
	ID         string     `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Hostname   string     `json:"hostname"`
	Username   string     `json:"username"`
	Shell      string     `json:"shell"`
	PID        int        `json:"pid"`
	EventCount int        `json:"event_count"`
}

// BatchStatus is the lifecycle state of a batch.
type BatchStatus string

const (
	BatchOpen        BatchStatus = "open"
	BatchPending     BatchStatus = "pending_summary"
	BatchSummarizing BatchStatus = "summarizing"
	BatchSummarized  BatchStatus = "summarized"
	BatchFailed      BatchStatus = "failed"
	BatchSkipped     BatchStatus = "skipped"
)

// Batch is a contiguous run of events in one session that will be summarized
// together.
type Batch struct {
	ID              string      `json:"id"`
	SessionID       string      `json:"session_id"`
	StartedAt       time.Time   `json:"started_at"`
	LastEventAt     time.Time   `json:"last_event_at"`
	ClosedAt        *time.Time  `json:"closed_at,omitempty"`
	Status          BatchStatus `json:"status"`
	Score           int         `json:"score"`
	MeaningfulCount int         `json:"meaningful_count"`
	EventCount      int         `json:"event_count"`
	TriggerReason   string      `json:"trigger_reason,omitempty"`
	Error           string      `json:"error,omitempty"`
	Attempts        int         `json:"attempts"`
	Events          []Event     `json:"events,omitempty"`
}

// Location addresses a place in the documentation hierarchy.
type Location struct {
	Space      string   `json:"space" yaml:"space"`
	ParentPath []string `json:"parent_path" yaml:"parent_path"`
	PageTitle  string   `json:"page_title,omitempty" yaml:"page_title,omitempty"`
}

// Step is one step of a runbook-style draft.
type Step struct {
	Description string   `json:"description"`
	Commands    []string `json:"commands"`
	Notes       string   `json:"notes"`
}

// Draft is the structured output the LLM produces for a batch. Its JSON
// schema lives in draft_schema.json and must stay in sync with this struct.
type Draft struct {
	Title             string   `json:"title"`
	Summary           string   `json:"summary"`
	Intent            string   `json:"intent"`
	DocKind           string   `json:"doc_kind"`
	Steps             []Step   `json:"steps"`
	Commands          []string `json:"commands"`
	AffectedSystems   []string `json:"affected_systems"`
	FilesTouched      []string `json:"files_touched"`
	Tags              []string `json:"tags"`
	SuggestedLocation Location `json:"suggested_location"`
	RelatedDocID      string   `json:"related_doc_id"`
	Confidence        string   `json:"confidence"`
	OpenQuestions     []string `json:"open_questions"`
}

// DraftStatus is the review state of a draft.
type DraftStatus string

const (
	DraftStatusDraft     DraftStatus = "draft"
	DraftStatusAccepted  DraftStatus = "accepted"
	DraftStatusPublished DraftStatus = "published"
	DraftStatusDismissed DraftStatus = "dismissed"
	DraftStatusStale     DraftStatus = "stale"
)

// DraftRecord is a draft as stored locally, with its review state and the
// markdown body the user may have edited.
type DraftRecord struct {
	ID          string          `json:"id"`
	BatchID     string          `json:"batch_id"`
	DocID       string          `json:"doc_id,omitempty"`
	Status      DraftStatus     `json:"status"`
	Title       string          `json:"title"`
	BodyMD      string          `json:"body_md"`
	Draft       Draft           `json:"draft"`
	Location    Location        `json:"location"`
	Tags        []string        `json:"tags"`
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	PromptHash  string          `json:"prompt_hash"`
	UsageIn     int             `json:"usage_in"`
	UsageOut    int             `json:"usage_out"`
	Hostname    string          `json:"hostname"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	PublishedAt *time.Time      `json:"published_at,omitempty"`
	PageRef     json.RawMessage `json:"page_ref,omitempty"`
}
