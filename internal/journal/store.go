package journal

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrOwnership  = errors.New("run ownership lost")
	ErrPending    = errors.New("pending commands require another provider request")
	ErrConflict   = errors.New("communication ID conflicts with accepted command")
	ErrForbidden  = errors.New("conversation is unavailable to principal")
	ErrBusy       = errors.New("conversation already has a live run")
	ErrState      = errors.New("run state does not permit operation")
	ErrDefinition = errors.New("pinned journey definition is unavailable")
)

type RunState string

const (
	RunAwaitingInput RunState = "awaiting_input"
	RunInterrupted   RunState = "interrupted"
	RunPending       RunState = "pending"
	RunRunning       RunState = "running"
	RunCompleted     RunState = "completed"
	RunFailed        RunState = "failed"
	RunAuthRequired  RunState = "auth_required"
	// RunDependencyUnavailable is terminal: a required dependency (tenant
	// directory, settings source or MCP endpoint) could not be used.
	RunDependencyUnavailable RunState = "dependency_unavailable"
	// RunLimitExceeded is terminal: a cost gateway rejected the run for a
	// budget or rate limit. Only FinishLimitExceeded may set it.
	RunLimitExceeded RunState = "limit_exceeded"
)

type Command struct{ ThreadID, RunID, CommunicationID, Principal, Tenant, Text, TargetJourney, InteractionID, ReplyKind, ReplyJSON string }
type Receipt struct {
	ThreadID, RunID, CommunicationID string
	ExecutionRunID                   string
	State                            RunState
	Answer                           string
}
type Event struct {
	Sequence                              int64
	Type, RunID, CallID, ToolName, Answer string
	CommunicationID                       string
	Reason                                string
	// Detail is an optional JSON object; empty is stored as NULL.
	Detail json.RawMessage
}
type CommandOutcome struct {
	CommunicationID, State, Reason string
}
type OperationOutcome struct {
	CallID, ToolName, Outcome string
}

type Run struct {
	Interaction       *Interaction
	CommandOutcomes   []CommandOutcome
	OperationOutcomes []OperationOutcome
	CommandRunIDs     []string
	PendingCommands   int
	RunID             string
	State             RunState
	JourneyID, Answer string
	// Scope and server info recorded by the owner; empty/nil when unrecorded.
	ProductVersion, EndpointID string
	Settings, ServerInfo       json.RawMessage
	// CatalogDigest is the catalog snapshot the run used; empty when unrecorded.
	CatalogDigest string
}

type Snapshot struct {
	Runs                       []Run
	ThreadID, Principal, RunID string
	RunState                   RunState
	JourneyID, Answer          string
	Watermark                  int64
	Events                     []Event
	// ActiveJourneys is the conversation's active set, least recently used first.
	ActiveJourneys []string
}
type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func lockConversation(ctx context.Context, tx pgx.Tx, thread, principal string) error {
	var owner string
	err := tx.QueryRow(ctx, "SELECT principal FROM agent_conversations WHERE id=$1 FOR UPDATE", thread).Scan(&owner)
	if err == pgx.ErrNoRows || err == nil && owner != principal {
		return ErrForbidden
	}
	return err
}
func appendEvent(ctx context.Context, tx pgx.Tx, thread string, e Event) error {
	var sequence int64
	if err := tx.QueryRow(ctx, "UPDATE agent_conversations SET sequence=sequence+1 WHERE id=$1 RETURNING sequence", thread).Scan(&sequence); err != nil {
		return err
	}
	var detail []byte // nil is NULL
	if len(e.Detail) > 0 {
		detail = e.Detail
	}
	_, err := tx.Exec(ctx, "INSERT INTO agent_events(thread_id,sequence,run_id,kind,call_id,tool_name,answer,communication_id,reason,detail) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", thread, sequence, e.RunID, e.Type, e.CallID, e.ToolName, e.Answer, e.CommunicationID, e.Reason, detail)
	return err
}
