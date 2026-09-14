// Package platform coordinates local execution against PostgreSQL owner leases.
package platform

import (
	"context"
	"crypto/rand"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

var (
	ErrConflict  = journal.ErrConflict
	ErrForbidden = journal.ErrForbidden
	ErrBusy      = journal.ErrBusy
)

type RunState = journal.RunState

const (
	RunAwaitingInput = journal.RunAwaitingInput
	RunInterrupted   = journal.RunInterrupted
	RunPending       = journal.RunPending
	RunRunning       = journal.RunRunning
	RunCompleted     = journal.RunCompleted
	RunFailed        = journal.RunFailed
	RunAuthRequired  = journal.RunAuthRequired
)

type Command struct {
	ThreadID, RunID, CommunicationID, Principal, Text, TargetJourney, InteractionID, ReplyKind, ReplyJSON string
}

func (c Command) durable() journal.Command {
	return journal.Command{ThreadID: c.ThreadID, RunID: c.RunID, CommunicationID: c.CommunicationID, Principal: c.Principal, Text: c.Text, TargetJourney: c.TargetJourney, InteractionID: c.InteractionID, ReplyKind: c.ReplyKind, ReplyJSON: c.ReplyJSON}
}

type Submission struct {
	Command Command
	Headers http.Header
}
type Receipt = journal.Receipt
type Event = journal.Event
type Snapshot = journal.Snapshot
type Run = journal.Run

type Platform struct {
	ownerID string
	runner  *agentruntime.Runner
	store   *journal.Store
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	workers sync.WaitGroup
}

func New(runner *agentruntime.Runner, store *journal.Store) *Platform {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Platform{runner: runner, store: store, ctx: ctx, cancel: cancel, ownerID: rand.Text()}
	p.workers.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := store.Reap(ctx); err != nil && ctx.Err() == nil {
					slog.ErrorContext(ctx, "run interruption check failed")
				}
			}
		}
	})
	return p
}

// Close stops local work. Lease expiry records interruption without transferring
// execution or claiming that an already dispatched remote call was canceled.
func (p *Platform) Close() { p.mu.Lock(); p.closed = true; p.cancel(); p.mu.Unlock(); p.workers.Wait() }
func (p *Platform) Submit(ctx context.Context, submission Submission) (Receipt, error) {
	c := submission.Command
	ctx, span := otel.Tracer("az-agent-platform/platform").Start(ctx, "agent.start_turn")
	span.SetAttributes(attribute.String("thread.id", c.ThreadID), attribute.String("run.id", c.RunID), attribute.String("communication.id", c.CommunicationID))
	defer span.End()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Receipt{}, context.Canceled
	}
	p.workers.Add(1)
	p.mu.Unlock()
	transferred := false
	defer func() {
		if !transferred {
			p.workers.Done()
		}
	}()
	receipt, fresh, err := p.store.Admit(ctx, c.durable())
	if err != nil {
		return Receipt{}, err
	}
	if !fresh && c.InteractionID != "" {
		snapshot, snapshotErr := p.store.Snapshot(ctx, c.ThreadID, c.Principal)
		if snapshotErr != nil {
			return Receipt{}, snapshotErr
		}
		for _, run := range snapshot.Runs {
			if run.RunID == c.RunID && run.State == RunPending {
				fresh = true
			}
		}
	}
	if fresh {
		target := c.TargetJourney
		if c.InteractionID != "" {
			snapshot, err := p.store.Snapshot(ctx, c.ThreadID, c.Principal)
			if err != nil {
				return Receipt{}, err
			}
			for _, run := range snapshot.Runs {
				if run.RunID == c.RunID {
					target = run.JourneyID
				}
			}
		}
		headers := p.runner.TransientHeaders(ctx, target, c.Text, submission.Headers)
		transferred = true
		go func() {
			defer p.workers.Done()
			// HTTP cancellation cannot revoke committed admission. Credentials remain in
			// this worker only and are never part of the durable command or journal.
			runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			defer cancel()
			stop := context.AfterFunc(p.ctx, cancel)
			defer stop()
			p.execute(runCtx, c, agentruntime.RunInput{ThreadID: c.ThreadID, RunID: c.RunID, Principal: c.Principal, Text: c.Text, TargetJourney: target, Headers: headers})
		}()
	}
	return receipt, nil
}
func (p *Platform) execute(ctx context.Context, c Command, in agentruntime.RunInput) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	owner, err := p.store.Claim(ctx, c.durable(), p.ownerID, journal.LeaseDuration)
	if err != nil {
		return
	}
	renewalDone := make(chan struct{})
	defer func() { cancel(); <-renewalDone }()
	go func() {
		defer close(renewalDone)
		ticker := time.NewTicker(journal.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, journal.LeaseDuration/3)
				err := p.store.Renew(renewCtx, owner, journal.LeaseDuration)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	in.Store = p.store
	in.Owner = owner
	journey, digest, err := p.runner.Binding(ctx, in)
	if err == nil {
		in.History, err = p.store.Start(ctx, owner, journey, digest)
		in.TargetJourney = journey
		in.DefinitionDigest = digest
	}
	for {
		var output agentruntime.RunOutput
		if err == nil {
			output, err = p.runner.Run(ctx, in)
		}
		if output.Waiting {
			return
		}
		if ctx.Err() != nil || errors.Is(err, journal.ErrOwnership) {
			return
		}
		state := RunCompleted
		if err != nil {
			state = RunFailed
			if errors.Is(err, platformmcp.ErrAuthRequired) {
				state = RunAuthRequired
			}
		}
		finishErr := p.store.Finish(ctx, owner, state, output.History, output.Answer, output.CallID, output.ToolName)
		if errors.Is(finishErr, journal.ErrPending) {
			// Admission won the final-drain race. Continue with this live owner's
			// credentials and in-memory history; no other replica can claim this run.
			in.History = output.History
			in.Iteration++
			continue
		}
		if finishErr != nil {
			slog.ErrorContext(ctx, "run completion commit failed", "run.id", c.RunID)
		}
		return
	}
}

func (p *Platform) Snapshot(ctx context.Context, thread, principal string) (Snapshot, error) {
	return p.store.Snapshot(ctx, thread, principal)
}

type Observation struct {
	Snapshot Snapshot
	Events   <-chan Event
	Errors   <-chan error
	Close    context.CancelFunc
}

// Observe tails the durable journal after a consistent snapshot. Polling is the
// entire wakeup mechanism here, so writes during snapshot creation cannot be lost.
func (p *Platform) Observe(ctx context.Context, thread, principal string, cursor int64) (*Observation, error) {
	if cursor < 0 {
		return nil, journal.ErrState
	}
	ctx, cancel := context.WithCancel(ctx)
	snapshot, err := p.store.Snapshot(ctx, thread, principal)
	if err != nil {
		cancel()
		return nil, err
	}
	after := max(cursor, snapshot.Watermark)
	events := make(chan Event)
	failures := make(chan error, 1)
	observation := &Observation{Snapshot: snapshot, Events: events, Errors: failures, Close: cancel}
	go func() {
		defer close(events)
		defer close(failures)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			batch, err := p.store.Events(ctx, thread, principal, after)
			if err != nil {
				if ctx.Err() == nil {
					failures <- err
				}
				return
			}
			for _, event := range batch {
				select {
				case events <- event:
					after = event.Sequence
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return observation, nil
}
