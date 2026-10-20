package journal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
)

var ErrOutcomeUnknown = errors.New("tool outcome unknown")

// Operation is one model-requested tool call. Name is the alias the model saw;
// OperationKey is the stable catalog key behind it ("" for callers that predate
// catalog keys). Approval is required exactly when RequiresApproval is set; only
// a read_only operation may leave it unset.
type Operation struct {
	CallID, Name, Arguments, Binding, Policy string
	OperationKey                             string
	// NameDigest and SchemaDigest are the binding v2 digests of the remote tool name and input schema.
	NameDigest, SchemaDigest string
	RequiresApproval         bool
	// JourneyID is the journey the call belongs to; "" falls back to the run's journey.
	JourneyID string
}

func validPolicy(policy string) bool {
	return policy == "read_only" || policy == "idempotent" || policy == "effectful"
}

// BeginAttempt commits the exact intent before dispatch; an unclosed intent is
// uncertain even if the process died in the gap before reaching the transport.
// A decided approval whose journey or binding no longer matches the call is marked
// superseded (that marker commits), nothing is written for the call, and ErrSuperseded is returned.
func (s *Store) BeginAttempt(ctx context.Context, o Owner, op Operation, attempt int, approval string) error {
	policy := op.Policy
	if op.CallID == "" || attempt < 1 || attempt > 2 || !validPolicy(policy) || policy != "read_only" && !op.RequiresApproval {
		return ErrState
	}
	requiresApproval := op.RequiresApproval
	superseded := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		c := o.Command
		var productVersion, definitionDigest *string
		var principal string
		if err := tx.QueryRow(ctx, "SELECT r.product_version,r.definition_digest,c.principal FROM agent_runs r JOIN agent_conversations c ON c.id=r.thread_id WHERE r.thread_id=$1 AND r.id=$2", c.ThreadID, c.RunID).Scan(&productVersion, &definitionDigest, &principal); err != nil {
			return err
		}
		if productVersion == nil || definitionDigest == nil {
			return ErrState
		}
		expected, err := BindCall(CallBinding{JourneyID: op.JourneyID, OperationKey: op.OperationKey, NameDigest: op.NameDigest, SchemaDigest: op.SchemaDigest,
			Arguments: json.RawMessage(op.Arguments), ProductVersion: *productVersion, Principal: principal, Conversation: c.ThreadID, DefinitionSet: *definitionDigest})
		if err != nil || expected != op.Binding {
			return ErrState
		}
		var unknown bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_operations WHERE thread_id=$1 AND outcome IN ('dispatching','outcome_unknown') AND call_id<>$2)", c.ThreadID, op.CallID).Scan(&unknown); err != nil {
			return err
		}
		if unknown {
			return ErrOutcomeUnknown
		}
		if attempt == 1 {
			if requiresApproval {
				var artifact, reply []byte
				var marker, journey string
				err := tx.QueryRow(ctx, "SELECT artifact,reply,COALESCE(marker,''),COALESCE(journey_id,'') FROM agent_interactions WHERE id=$1 AND thread_id=$2 AND continuation_run_id=$3 AND kind='approval' AND state='consumed' AND expires_at>clock_timestamp() FOR UPDATE", approval, c.ThreadID, c.RunID).Scan(&artifact, &reply, &marker, &journey)
				if err != nil {
					return ErrState
				}
				var i Interaction
				var answer Reply
				if json.Unmarshal(artifact, &i) != nil || json.Unmarshal(reply, &answer) != nil || answer.Decision != "approve" || i.Call.CallID != op.CallID || i.Call.Name != op.Name {
					return ErrState
				}
				if marker != "decided" {
					return ErrState
				}
				if journey != op.JourneyID || i.Call.Binding != expected {
					if err := supersede(ctx, tx, c.ThreadID, c.RunID, approval); err != nil {
						return err
					}
					superseded = true
					return nil
				}
			}
			if _, err := tx.Exec(ctx, "INSERT INTO agent_operations(call_id,thread_id,run_id,alias,operation_key,arguments,binding,policy,requires_approval,outcome,journey_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'dispatching',COALESCE(NULLIF($10,''),(SELECT journey_id FROM agent_runs WHERE thread_id=$2 AND id=$3)))", op.CallID, c.ThreadID, c.RunID, op.Name, op.OperationKey, op.Arguments, op.Binding, policy, requiresApproval, op.JourneyID); err != nil {
				return err
			}
			if requiresApproval {
				if _, err := tx.Exec(ctx, "UPDATE agent_interactions SET marker='dispatched' WHERE id=$1 AND thread_id=$2", approval, c.ThreadID); err != nil {
					return err
				}
			}
		} else {
			var previous, stored, binding string
			if err := tx.QueryRow(ctx, "SELECT outcome,policy,binding FROM agent_operations WHERE call_id=$1 AND thread_id=$2 AND run_id=$3", op.CallID, c.ThreadID, c.RunID).Scan(&previous, &stored, &binding); err != nil {
				return err
			}
			if binding != op.Binding || stored != policy || (previous != "rejected" && previous != "outcome_unknown") || policy == "effectful" {
				return ErrState
			}
			if _, err := tx.Exec(ctx, "UPDATE agent_operations SET outcome='dispatching' WHERE call_id=$1", op.CallID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO agent_attempts(call_id,attempt,outcome) VALUES($1,$2,'dispatching')", op.CallID, attempt); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "tool.started", CallID: op.CallID, ToolName: op.Name})
	})
	if err == nil && superseded {
		return ErrSuperseded
	}
	return err
}
func (s *Store) EndAttempt(ctx context.Context, o Owner, op Operation, attempt int, outcome string) error {
	switch outcome {
	case "completed", "rejected", "auth_required", "outcome_unknown":
	default:
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, "UPDATE agent_operations SET outcome=$4 WHERE call_id=$1 AND thread_id=$2 AND run_id=$3 AND outcome='dispatching'", op.CallID, o.Command.ThreadID, o.Command.RunID, outcome)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrState
		}
		result, err = tx.Exec(ctx, "UPDATE agent_attempts SET outcome=$3 WHERE call_id=$1 AND attempt=$2 AND outcome='dispatching'", op.CallID, attempt, outcome)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrState
		}
		return appendEvent(ctx, tx, o.Command.ThreadID, Event{RunID: o.Command.RunID, Type: "tool." + outcome, CallID: op.CallID, ToolName: op.Name})
	})
}

func markOperationsUnknown(ctx context.Context, tx pgx.Tx, thread, run string) error {
	if _, err := tx.Exec(ctx, "UPDATE agent_attempts a SET outcome='outcome_unknown' FROM agent_operations o WHERE a.call_id=o.call_id AND o.thread_id=$1 AND o.run_id=$2 AND a.outcome='dispatching'", thread, run); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "UPDATE agent_operations SET outcome='outcome_unknown' WHERE thread_id=$1 AND run_id=$2 AND outcome='dispatching' RETURNING call_id,alias", thread, run)
	if err != nil {
		return err
	}
	var operations []Operation
	for rows.Next() {
		var op Operation
		if err := rows.Scan(&op.CallID, &op.Name); err != nil {
			rows.Close()
			return err
		}
		operations = append(operations, op)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, op := range operations {
		if err := appendEvent(ctx, tx, thread, Event{RunID: run, Type: "tool.outcome_unknown", CallID: op.CallID, ToolName: op.Name}); err != nil {
			return err
		}
	}
	return nil
}
