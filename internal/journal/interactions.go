package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"io"
	"strings"
	"time"
)

// ErrSuperseded reports that a decided approval no longer matches the call it was given for;
// the approval is marked superseded and nothing was dispatched.
var ErrSuperseded = errors.New("approval superseded")

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []Option `json:"options"`
}
type ProposedCall struct {
	CallID         string          `json:"callId"`
	ProviderCallID string          `json:"providerCallId"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Binding        string          `json:"binding"`
}
type Interaction struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Questions []Question   `json:"questions,omitempty"`
	Call      ProposedCall `json:"call"`
	ExpiresAt time.Time    `json:"expiresAt"`
	// JourneyID is the journey that owns the interaction ("" for the run's journey, or none).
	// It is stored in its own column and never appears in the artifact.
	JourneyID string `json:"-"`
	// OperationKey, NameDigest and SchemaDigest are the binding v2 inputs; own columns, never in the artifact.
	OperationKey string `json:"-"`
	NameDigest   string `json:"-"`
	SchemaDigest string `json:"-"`
}
type Answer struct {
	Option string `json:"option,omitempty"`
	Custom string `json:"custom,omitempty"`
}
type Reply struct {
	Answers  map[string]Answer `json:"answers,omitempty"`
	Decision string            `json:"decision,omitempty"`
	Binding  string            `json:"binding,omitempty"`
}
type Continuation struct {
	Interaction Interaction
	Checkpoint  json.RawMessage
	History     json.RawMessage
	Reply       Reply
	// Marker is the approval marker ("" for a clarification).
	Marker string
}

func DecodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(value); err != nil {
		return ErrState
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrState
	}
	return nil
}

func (i Interaction) Validate() error {
	if i.ID == "" || i.Call.CallID == "" || i.Call.ProviderCallID == "" || i.Call.Name == "" {
		return ErrState
	}
	if i.Call.Binding == "" {
		return ErrState
	}
	if i.Kind == "approval" {
		if len(i.Questions) != 0 {
			return ErrState
		}
		return nil
	}
	if i.Kind != "clarification" || len(i.Questions) < 1 || len(i.Questions) > 3 {
		return ErrState
	}
	seen := map[string]bool{}
	for _, q := range i.Questions {
		if q.ID == "" || seen[q.ID] || strings.TrimSpace(q.Question) == "" || q.Header == "" || len([]rune(q.Header)) > 12 || len(q.Options) < 2 || len(q.Options) > 3 {
			return ErrState
		}
		seen[q.ID] = true
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || strings.TrimSpace(o.Description) == "" || labels[o.Label] {
				return ErrState
			}
			labels[o.Label] = true
		}
	}
	return nil
}
func (i Interaction) ValidateReply(r Reply) error {
	if i.Kind == "approval" {
		if len(r.Answers) != 0 || r.Binding != i.Call.Binding || (r.Decision != "approve" && r.Decision != "deny") {
			return ErrState
		}
		return nil
	}
	if r.Decision != "" || r.Binding != "" || len(r.Answers) != len(i.Questions) {
		return ErrState
	}
	for _, q := range i.Questions {
		a, ok := r.Answers[q.ID]
		if !ok || (a.Option == "") == (strings.TrimSpace(a.Custom) == "") {
			return ErrState
		}
		if a.Custom != "" {
			if len(a.Custom) > 4096 {
				return ErrState
			}
			continue
		}
		found := false
		for _, o := range q.Options {
			found = found || a.Option == o.Label
		}
		if !found {
			return ErrState
		}
	}
	return nil
}

// Wait is the sole planned continuation boundary: all state commits with owner release.
func (s *Store) Wait(ctx context.Context, o Owner, i Interaction, checkpoint, history json.RawMessage) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if !json.Valid(checkpoint) || !json.Valid(history) {
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		c := o.Command
		var pending bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_commands WHERE thread_id=$1 AND execution_run_id=$2 AND NOT included AND terminal_reason='')", c.ThreadID, c.RunID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrPending
		}
		if i.Kind == "approval" && (i.JourneyID == "" || i.OperationKey == "") {
			return ErrState
		}
		var productVersion, definitionDigest *string
		var principal string
		var active, recorded []byte
		if err := tx.QueryRow(ctx, "SELECT r.product_version,r.definition_digest,c.principal,c.active_journeys,r.settings FROM agent_runs r JOIN agent_conversations c ON c.id=r.thread_id WHERE r.thread_id=$1 AND r.id=$2", c.ThreadID, c.RunID).Scan(&productVersion, &definitionDigest, &principal, &active, &recorded); err != nil {
			return err
		}
		if productVersion == nil || definitionDigest == nil {
			return ErrState
		}
		ttl, err := recordedInteractionTTL(recorded)
		if err != nil {
			return err
		}
		expected, err := BindCall(CallBinding{JourneyID: i.JourneyID, OperationKey: i.OperationKey, NameDigest: i.NameDigest, SchemaDigest: i.SchemaDigest,
			Arguments: i.Call.Arguments, ProductVersion: *productVersion, Principal: principal, Conversation: c.ThreadID, DefinitionSet: *definitionDigest})
		if err != nil || expected != i.Call.Binding {
			return ErrState
		}
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()+$1*interval '1 millisecond'", ttl.Milliseconds()).Scan(&i.ExpiresAt); err != nil {
			return err
		}
		artifact, err := json.Marshal(i)
		if err != nil {
			return err
		}
		var marker any
		if i.Kind == "approval" {
			marker = "requested"
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at,marker,operation_key,name_digest,schema_digest,product_version,catalog_digest,active_journeys) SELECT $3,thread_id,id,COALESCE(NULLIF($9,''),journey_id),definition_digest,$4,$5,$6,$7,$8,$10,$11,$12,$13,product_version,catalog_digest,$14 FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, i.ID, i.Kind, artifact, checkpoint, history, i.ExpiresAt, i.JourneyID, marker, i.OperationKey, i.NameDigest, i.SchemaDigest, active); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE agent_conversations SET history=$2 WHERE id=$1", c.ThreadID, history); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE agent_runs SET state='awaiting_input',owner_id='',lease_until=clock_timestamp() WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "run.awaiting_input", CallID: i.Call.CallID})
	})
}

// recordedInteractionTTL reads the wait cap from the settings recorded on the run; runs recorded
// before the setting existed (or with no value) get the built-in default.
func recordedInteractionTTL(recorded []byte) (time.Duration, error) {
	if len(recorded) == 0 {
		return settings.DefaultInteractionTTL, nil
	}
	var r struct {
		InteractionTTL time.Duration `json:"interaction_ttl"`
	}
	if err := json.Unmarshal(recorded, &r); err != nil {
		return 0, ErrState
	}
	if r.InteractionTTL <= 0 {
		return settings.DefaultInteractionTTL, nil
	}
	return r.InteractionTTL, nil
}
func expireInteractions(ctx context.Context, tx pgx.Tx, thread string) error {
	rows, err := tx.Query(ctx, "UPDATE agent_interactions SET state='expired',marker=CASE WHEN kind='approval' THEN 'denied' END WHERE thread_id=$1 AND state='pending' AND expires_at<=clock_timestamp() RETURNING run_id", thread)
	if err != nil {
		return err
	}
	var runs []string
	for rows.Next() {
		var run string
		if err := rows.Scan(&run); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, run := range runs {
		var artifact, history []byte
		if err := tx.QueryRow(ctx, "SELECT artifact,history FROM agent_interactions WHERE thread_id=$1 AND run_id=$2 AND state='expired'", thread, run).Scan(&artifact, &history); err != nil {
			return err
		}
		var i Interaction
		var msgs []*message.Message
		if json.Unmarshal(artifact, &i) != nil || json.Unmarshal(history, &msgs) != nil {
			return ErrState
		}
		msgs = append(msgs, &message.Message{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: i.Call.ProviderCallID, Result: map[string]string{"error": "interaction_expired"}}}})
		closed, err := json.Marshal(msgs)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE agent_conversations SET history=$2 WHERE id=$1", thread, closed); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET state='failed' WHERE thread_id=$1 AND id=$2 AND state='awaiting_input'", thread, run); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, thread, Event{RunID: run, Type: "interaction.expired"}); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, thread, Event{RunID: run, Type: "run.failed", Reason: ReasonInteractionExpired}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) reply(ctx context.Context, c Command) (Receipt, bool, error) {
	receipt := Receipt{ThreadID: c.ThreadID, RunID: c.RunID, CommunicationID: c.CommunicationID, ExecutionRunID: c.RunID, State: RunPending}
	fresh := false
	expired := false
	if c.ThreadID == "" || c.RunID == "" || c.Principal == "" || c.CommunicationID == "" || c.Text != "" {
		return Receipt{}, false, ErrState
	}
	var answer Reply
	if err := DecodeStrict([]byte(c.ReplyJSON), &answer); err != nil {
		return Receipt{}, false, err
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
			return err
		}
		var previous []byte
		err := tx.QueryRow(ctx, "SELECT payload,execution_run_id FROM agent_commands WHERE thread_id=$1 AND communication_id=$2", c.ThreadID, c.CommunicationID).Scan(&previous, &receipt.ExecutionRunID)
		if err == nil {
			var original Command
			if err := json.Unmarshal(previous, &original); err != nil {
				return err
			}
			if original != c {
				return ErrConflict
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		var artifact []byte
		var state, journey, oldRun string
		var valid bool
		err = tx.QueryRow(ctx, "SELECT artifact,state,COALESCE(journey_id,''),run_id FROM agent_interactions WHERE thread_id=$1 AND id=$2 FOR UPDATE", c.ThreadID, c.InteractionID).Scan(&artifact, &state, &journey, &oldRun)
		if err == pgx.ErrNoRows {
			return ErrState
		}
		if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, "SELECT expires_at>clock_timestamp() FROM agent_interactions WHERE thread_id=$1 AND id=$2", c.ThreadID, c.InteractionID).Scan(&valid); err != nil {
			return err
		}
		if !valid && state == "pending" {
			expired = true
			return expireInteractions(ctx, tx, c.ThreadID)
		}
		if state == "expired" {
			return ErrExpired
		}
		if !valid || state != "pending" {
			return ErrConflict
		}
		var i Interaction
		if err = json.Unmarshal(artifact, &i); err != nil {
			return err
		}
		if i.Kind != c.ReplyKind || (c.TargetJourney != "" && journey != "" && c.TargetJourney != journey) {
			return ErrState
		}
		if err = i.ValidateReply(answer); err != nil {
			return err
		}
		payload, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES($1,$2,$3,$4,$3)", c.ThreadID, c.CommunicationID, c.RunID, payload); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) SELECT $3,thread_id,'pending',journey_id,definition_digest FROM agent_interactions WHERE thread_id=$1 AND id=$2", c.ThreadID, c.InteractionID, c.RunID); err != nil {
			return err
		}
		var marker any
		if i.Kind == "approval" {
			marker = "denied"
			if answer.Decision == "approve" {
				marker = "decided"
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE agent_interactions SET state='consumed',continuation_run_id=$3,reply=$4,marker=$5 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.InteractionID, c.RunID, []byte(c.ReplyJSON), marker); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE agent_runs SET state='completed' WHERE thread_id=$1 AND id=$2 AND state='awaiting_input'", c.ThreadID, oldRun); err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, c.ThreadID, Event{RunID: oldRun, Type: "interaction.consumed"}); err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "command.accepted", CommunicationID: c.CommunicationID}); err != nil {
			return err
		}
		if i.Kind == "approval" && answer.Decision == "approve" {
			if _, err = tx.Exec(ctx, "UPDATE agent_commands SET terminal_reason='consumed' WHERE thread_id=$1 AND communication_id=$2", c.ThreadID, c.CommunicationID); err != nil {
				return err
			}
			if err = appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "command.consumed", CommunicationID: c.CommunicationID}); err != nil {
				return err
			}
		}
		fresh = true
		return nil
	})
	if err == nil && expired {
		err = ErrExpired
	}
	return receipt, fresh, err
}
func (s *Store) Continuation(ctx context.Context, o Owner) (*Continuation, error) {
	if o.Command.InteractionID == "" {
		return nil, nil
	}
	var result Continuation
	var artifact, reply []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT artifact,checkpoint,history,reply,COALESCE(journey_id,''),COALESCE(operation_key,''),COALESCE(name_digest,''),COALESCE(schema_digest,''),COALESCE(marker,'') FROM agent_interactions WHERE thread_id=$1 AND id=$2 AND continuation_run_id=$3 AND state='consumed'", o.Command.ThreadID, o.Command.InteractionID, o.Command.RunID).Scan(&artifact, &result.Checkpoint, &result.History, &reply,
			&result.Interaction.JourneyID, &result.Interaction.OperationKey, &result.Interaction.NameDigest, &result.Interaction.SchemaDigest, &result.Marker)
	})
	if err != nil {
		return nil, err
	}
	// the artifact never carries the column-backed fields, so unmarshalling leaves them intact
	if err = json.Unmarshal(artifact, &result.Interaction); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(reply, &result.Reply); err != nil {
		return nil, err
	}
	return &result, nil
}
func (s *Store) Interaction(ctx context.Context, thread, principal, run string) (*Interaction, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, "SELECT artifact FROM agent_interactions i JOIN agent_conversations c ON c.id=i.thread_id WHERE i.thread_id=$1 AND c.principal=$2 AND i.run_id=$3 AND i.state='pending'", thread, principal, run).Scan(&data)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var i Interaction
	if err = json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("interaction: %w", err)
	}
	return &i, nil
}

// Supersede marks an approval consumed by this run with marker 'decided' as superseded and appends
// interaction.superseded. Fenced. Already superseded -> nil; any other marker -> ErrState.
func (s *Store) Supersede(ctx context.Context, o Owner, interactionID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		return supersede(ctx, tx, o.Command.ThreadID, o.Command.RunID, interactionID)
	})
}

func supersede(ctx context.Context, tx pgx.Tx, thread, run, interactionID string) error {
	var marker, callID string
	err := tx.QueryRow(ctx, "SELECT marker,COALESCE(artifact->'call'->>'callId','') FROM agent_interactions WHERE id=$1 AND thread_id=$2 AND continuation_run_id=$3 AND kind='approval' AND state='consumed' FOR UPDATE", interactionID, thread, run).Scan(&marker, &callID)
	if err == pgx.ErrNoRows {
		return ErrState
	}
	if err != nil {
		return err
	}
	if marker == "superseded" {
		return nil
	}
	if marker != "decided" {
		return ErrState
	}
	if _, err = tx.Exec(ctx, "UPDATE agent_interactions SET marker='superseded' WHERE id=$1 AND thread_id=$2", interactionID, thread); err != nil {
		return err
	}
	return appendEvent(ctx, tx, thread, Event{RunID: run, Type: "interaction.superseded", CallID: callID})
}
