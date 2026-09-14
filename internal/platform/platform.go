package platform

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"

	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

var (
	ErrConflict        = errors.New("communication ID conflicts with accepted command")
	ErrForbidden       = errors.New("thread belongs to another principal")
	ErrExecutionFailed = errors.New("accepted run failed")
)

type RunState string

const (
	RunAccepted  RunState = "accepted"
	RunRunning   RunState = "running"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
)

type Command struct {
	ThreadID        string
	RunID           string
	CommunicationID string
	Principal       string
	Text            string
	TargetJourney   string
}

type Submission struct {
	Command Command
	Headers http.Header
}

type Receipt struct {
	ThreadID        string
	RunID           string
	CommunicationID string
	State           RunState
	Answer          string
}

type Event struct {
	Sequence int64
	Type     string
	RunID    string
	CallID   string
	ToolName string
}

type Snapshot struct {
	ThreadID  string
	Principal string
	RunID     string
	RunState  RunState
	JourneyID string
	Answer    string
	Events    []Event
}

type Submitter interface {
	Submit(context.Context, Submission) (Receipt, error)
}

type Platform struct {
	mu      sync.Mutex
	runner  *agentruntime.Runner
	threads map[string]*thread
}

type thread struct {
	principal string
	commands  map[string]acceptedCommand
	snapshot  Snapshot
}

type acceptedCommand struct {
	command Command
	receipt Receipt
}

func New(runner *agentruntime.Runner) *Platform {
	return &Platform{runner: runner, threads: make(map[string]*thread)}
}

func (p *Platform) Submit(ctx context.Context, submission Submission) (Receipt, error) {
	// ponytail: Task 1 serializes its process-local fixture; Task 2 replaces this with PostgreSQL transactions.
	p.mu.Lock()
	defer p.mu.Unlock()
	command := submission.Command
	current := p.threads[command.ThreadID]
	if current != nil && current.principal != command.Principal {
		return Receipt{}, ErrForbidden
	}
	if current == nil {
		current = &thread{principal: command.Principal, commands: make(map[string]acceptedCommand)}
		current.snapshot = Snapshot{ThreadID: command.ThreadID, Principal: command.Principal}
		p.threads[command.ThreadID] = current
	}
	if accepted, ok := current.commands[command.CommunicationID]; ok {
		if accepted.command != command {
			return Receipt{}, ErrConflict
		}
		if accepted.receipt.State == RunFailed {
			return accepted.receipt, ErrExecutionFailed
		}
		return accepted.receipt, nil
	}

	receipt := Receipt{ThreadID: command.ThreadID, RunID: command.RunID, CommunicationID: command.CommunicationID, State: RunAccepted}
	current.commands[command.CommunicationID] = acceptedCommand{command: command, receipt: receipt}
	current.snapshot.RunID = command.RunID
	current.snapshot.RunState = RunRunning
	p.append(current, Event{Type: "command.accepted", RunID: command.RunID})
	p.append(current, Event{Type: "run.started", RunID: command.RunID})

	output, err := p.runner.Run(ctx, agentruntime.RunInput{
		ThreadID: command.ThreadID, RunID: command.RunID, Principal: command.Principal,
		Text: command.Text, TargetJourney: command.TargetJourney, Headers: submission.Headers,
	})
	if err != nil {
		current.snapshot.RunState = RunFailed
		p.append(current, Event{Type: "run.failed", RunID: command.RunID})
		receipt.State = RunFailed
		current.commands[command.CommunicationID] = acceptedCommand{command: command, receipt: receipt}
		return receipt, err
	}
	if output.CallID != "" {
		p.append(current, Event{Type: "tool.completed", RunID: command.RunID, CallID: output.CallID, ToolName: output.ToolName})
	}
	current.snapshot.JourneyID = output.JourneyID
	current.snapshot.Answer = output.Answer
	current.snapshot.RunState = RunCompleted
	p.append(current, Event{Type: "message.completed", RunID: command.RunID})
	p.append(current, Event{Type: "run.completed", RunID: command.RunID})
	receipt.State = RunCompleted
	receipt.Answer = output.Answer
	current.commands[command.CommunicationID] = acceptedCommand{command: command, receipt: receipt}
	return receipt, nil
}

func (p *Platform) Snapshot(threadID, principal string) (Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.threads[threadID]
	if current == nil || current.principal != principal {
		return Snapshot{}, ErrForbidden
	}
	snapshot := current.snapshot
	snapshot.Events = slices.Clone(snapshot.Events)
	return snapshot, nil
}

func (p *Platform) append(current *thread, event Event) {
	event.Sequence = int64(len(current.snapshot.Events) + 1)
	current.snapshot.Events = append(current.snapshot.Events, event)
}
