// Package platform executes committed conversation commands in one development
// process. PostgreSQL owns durable truth; Task 3 adds recoverable owner leases.
package platform

import (
	"context"
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
	RunPending      = journal.RunPending
	RunRunning      = journal.RunRunning
	RunCompleted    = journal.RunCompleted
	RunFailed       = journal.RunFailed
	RunAuthRequired = journal.RunAuthRequired
)

type Command struct {
	ThreadID, RunID, CommunicationID, Principal, Text, TargetJourney string
}

func (c Command) durable() journal.Command {
	return journal.Command{ThreadID: c.ThreadID, RunID: c.RunID, CommunicationID: c.CommunicationID, Principal: c.Principal, Text: c.Text, TargetJourney: c.TargetJourney}
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
	return &Platform{runner: runner, store: store, ctx: ctx, cancel: cancel}
}

// Close cancels local execution, leaving interrupted running records durable.
// It provides no restart/takeover guarantee; interrupted owners belong to Task 3.
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
	if fresh {
		headers := p.runner.TransientHeaders(ctx, c.TargetJourney, c.Text, submission.Headers)
		transferred = true
		go func() {
			defer p.workers.Done()
			// HTTP cancellation cannot revoke committed admission. Credentials remain in
			// this worker only and are never part of the durable command or journal.
			runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			defer cancel()
			stop := context.AfterFunc(p.ctx, cancel)
			defer stop()
			p.execute(runCtx, c, headers)
		}()
	}
	return receipt, nil
}
func (p *Platform) execute(ctx context.Context, c Command, headers http.Header) {
	in := agentruntime.RunInput{ThreadID: c.ThreadID, RunID: c.RunID, Principal: c.Principal, Text: c.Text, TargetJourney: c.TargetJourney, Headers: headers}
	journey, digest, err := p.runner.Binding(ctx, in)
	if err == nil {
		in.History, err = p.store.Start(ctx, c.durable(), journey, digest)
	}
	var output agentruntime.RunOutput
	if err == nil {
		output, err = p.runner.Run(ctx, in)
	}
	if ctx.Err() != nil {
		return
	}
	state := RunCompleted
	if err != nil {
		state = RunFailed
		if errors.Is(err, platformmcp.ErrAuthRequired) {
			state = RunAuthRequired
		}
	}
	// If this commit fails, the last committed state remains running. No answer is
	// emitted until committed; durable recovery is explicitly outside this slice.
	if err := p.store.Finish(ctx, c.durable(), state, output.History, output.Answer, output.CallID, output.ToolName); err != nil {
		slog.ErrorContext(ctx, "run completion commit failed", "run.id", c.RunID)
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
