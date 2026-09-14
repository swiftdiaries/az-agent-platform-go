package journal

import (
	"context"
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
)

type Command struct{ ThreadID, RunID, CommunicationID, Principal, Text, TargetJourney, InteractionID, ReplyKind, ReplyJSON string }
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
}
type CommandOutcome struct {
	CommunicationID, State, Reason string
}

type Run struct {
	Interaction       *Interaction
	CommandOutcomes   []CommandOutcome
	CommandRunIDs     []string
	PendingCommands   int
	RunID             string
	State             RunState
	JourneyID, Answer string
}

type Snapshot struct {
	Runs                       []Run
	ThreadID, Principal, RunID string
	RunState                   RunState
	JourneyID, Answer          string
	Watermark                  int64
	Events                     []Event
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
	_, err := tx.Exec(ctx, "INSERT INTO agent_events(thread_id,sequence,run_id,kind,call_id,tool_name,answer,communication_id,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", thread, sequence, e.RunID, e.Type, e.CallID, e.ToolName, e.Answer, e.CommunicationID, e.Reason)
	return err
}
