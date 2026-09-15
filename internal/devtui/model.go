package devtui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

const (
	maxTranscriptLines = 1000
	maxTranscriptBytes = 256 << 10
)

type question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []option `json:"options"`
}

type option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type proposedCall struct {
	CallID    string          `json:"callId"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Binding   string          `json:"binding"`
}

type interaction struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Questions []question   `json:"questions"`
	Call      proposedCall `json:"call"`
}

type Model struct {
	client          Client
	threadID, runID string
	journeyID       string
	input           textinput.Model
	viewport        viewport.Model
	transcript      []string
	pending         *interaction
	answers         map[string]map[string]string
	questionIndex   int
	status          string
	streaming       bool
	events          chan tea.Msg
	cancel          context.CancelFunc
	resumeRun       string
	terminal        bool
	lastRequest     *Request
	screenHeight    int
}

type streamEventMsg struct{ event Event }
type streamDoneMsg struct{ err error }

func NewModel(client Client, threadID, runID, journeyID string) *Model {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "Message (/new, /journey <id>)"
	input.CharLimit = 4000
	return &Model{
		client: client, threadID: threadID, runID: runID, journeyID: journeyID, resumeRun: runID,
		input: input, viewport: viewport.New(viewport.WithWidth(80), viewport.WithHeight(18)),
		status: "ready", events: make(chan tea.Msg),
	}
}

func (m *Model) Init() tea.Cmd {
	if m.resumeRun != "" {
		return tea.Batch(m.input.Focus(), m.startResume(m.resumeRun))
	}
	return m.input.Focus()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.input.SetWidth(max(1, msg.Width-2))
		m.viewport.SetWidth(max(1, msg.Width))
		m.screenHeight = msg.Height
		m.layout()
		m.refresh()
		return m, nil
	case streamEventMsg:
		m.apply(msg.event)
		return m, waitForStream(m.events)
	case streamDoneMsg:
		m.streaming = false
		m.cancel = nil
		if msg.err != nil {
			m.status = "error: " + sanitize(msg.err.Error())
			if m.canRetry() {
				m.status += "; type /retry"
			}
		} else if !m.terminal {
			m.status = "stream ended; type /retry"
		} else if m.pending != nil {
			m.status = "awaiting input"
		} else if m.status == "streaming" {
			m.status = "finished"
		}
		m.refresh()
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		if key == "pgup" || key == "pgdown" {
			var command tea.Cmd
			m.viewport, command = m.viewport.Update(msg)
			return m, command
		}
		if key == "enter" && !m.streaming {
			command := m.submit()
			return m, command
		}
	}
	var command tea.Cmd
	m.input, command = m.input.Update(msg)
	return m, command
}

func (m *Model) View() tea.View {
	m.layout()
	m.refresh()
	footer := "PgUp/PgDown scroll · Ctrl+C quit"
	if m.canRetry() {
		footer = "/retry resend last request · " + footer
	}
	return tea.NewView(fmt.Sprintf("%s\n%s\n%s\n%s", m.header(), m.viewport.View(), m.input.View(), footer))
}

func (m *Model) apply(event Event) {
	if event.Type == "CUSTOM" {
		var custom struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(event.Data, &custom) == nil && custom.Name != "" {
			event.Type, event.Data = custom.Name, custom.Value
		}
	}
	var payload struct {
		Delta    string `json:"delta"`
		Message  string `json:"message"`
		Snapshot struct {
			ThreadID    string       `json:"threadId"`
			RunState    string       `json:"runState"`
			Answer      string       `json:"answer"`
			Interaction *interaction `json:"interaction"`
		} `json:"snapshot"`
		ToolName string `json:"toolName"`
	}
	_ = json.Unmarshal(event.Data, &payload)
	switch event.Type {
	case "TEXT_MESSAGE_CONTENT":
		m.add("assistant: " + sanitize(payload.Delta))
	case "STATE_SNAPSHOT":
		if payload.Snapshot.ThreadID != "" {
			m.threadID = payload.Snapshot.ThreadID
		}
		m.pending, m.answers, m.questionIndex = payload.Snapshot.Interaction, map[string]map[string]string{}, 0
		if payload.Snapshot.RunState == "awaiting_input" && m.pending != nil {
			m.status = "awaiting input"
		}
	case "interaction.requested":
		var pending interaction
		if json.Unmarshal(event.Data, &pending) == nil && pending.ID != "" {
			m.pending, m.answers, m.questionIndex = &pending, map[string]map[string]string{}, 0
			m.status = "awaiting input"
		}
	case "RUN_FINISHED":
		m.terminal = true
		if m.pending == nil {
			m.status = "finished"
		}
	case "RUN_ERROR":
		m.terminal = true
		m.status = "error: " + sanitize(payload.Message)
	default:
		if strings.HasPrefix(event.Type, "tool.") {
			name := payload.ToolName
			if name == "" {
				name = "operation"
			}
			m.add(sanitize(event.Type) + ": " + sanitize(name))
		}
	}
	m.refresh()
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.Reset()
	if text == "/new" {
		m.threadID, m.runID, m.pending, m.answers, m.lastRequest = "", "", nil, nil, nil
		m.terminal = false
		m.transcript = nil
		m.status = "new conversation"
		m.refresh()
		return nil
	}
	if text == "/retry" {
		if m.lastRequest != nil && !m.terminal {
			return m.start(*m.lastRequest)
		}
		if m.runID != "" && !m.terminal {
			return m.startResume(m.runID)
		}
		m.status = "nothing to retry"
		return nil
	}
	if m.lastRequest != nil && !m.terminal {
		m.status = "stream ended; type /retry"
		return nil
	}
	if journey, ok := strings.CutPrefix(text, "/journey "); ok && strings.TrimSpace(journey) != "" {
		m.journeyID = strings.TrimSpace(journey)
		m.status = "journey: " + sanitize(m.journeyID)
		m.refresh()
		return nil
	}
	if m.pending != nil {
		return m.submitInteraction(text)
	}
	if m.threadID == "" {
		m.threadID = newID()
	}
	m.runID = newID()
	m.add("you: " + sanitize(text))
	return m.start(Request{ThreadID: m.threadID, RunID: m.runID, JourneyID: m.journeyID, Text: text})
}

func (m *Model) submitInteraction(text string) tea.Cmd {
	pending := m.pending
	if pending.Kind == "approval" {
		decision := strings.ToLower(text)
		if decision != "approve" && decision != "deny" {
			m.status = "type approve or deny"
			return nil
		}
		m.runID = newID()
		return m.start(Request{ThreadID: m.threadID, RunID: m.runID, Reply: &InteractionReply{InteractionID: pending.ID, Kind: "approval", Answer: map[string]string{"decision": decision, "binding": pending.Call.Binding}}})
	}
	if m.questionIndex < len(pending.Questions) {
		question := pending.Questions[m.questionIndex]
		answer := map[string]string{"custom": text}
		for _, option := range question.Options {
			if text == option.Label {
				answer = map[string]string{"option": text}
				break
			}
		}
		m.answers[question.ID] = answer
		m.questionIndex++
	}
	if m.questionIndex < len(pending.Questions) {
		m.refresh()
		return nil
	}
	m.runID = newID()
	return m.start(Request{ThreadID: m.threadID, RunID: m.runID, Reply: &InteractionReply{InteractionID: pending.ID, Kind: "clarification", Answer: map[string]any{"answers": m.answers}}})
}

func (m *Model) start(request Request) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.streaming, m.status, m.terminal = cancel, true, "streaming", false
	m.lastRequest = &request
	go func(events chan tea.Msg, client Client) {
		err := client.Stream(ctx, request, func(event Event) {
			select {
			case events <- streamEventMsg{event}:
			case <-ctx.Done():
			}
		})
		select {
		case events <- streamDoneMsg{err}:
		case <-ctx.Done():
		}
	}(m.events, m.client)
	return waitForStream(m.events)
}

func (m *Model) startResume(runID string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.streaming, m.status, m.terminal = cancel, true, "resuming", false
	go func(events chan tea.Msg, client Client) {
		err := client.Resume(ctx, m.threadID, runID, func(event Event) {
			select {
			case events <- streamEventMsg{event}:
			case <-ctx.Done():
			}
		})
		select {
		case events <- streamDoneMsg{err}:
		case <-ctx.Done():
		}
	}(m.events, m.client)
	return waitForStream(m.events)
}

func waitForStream(events <-chan tea.Msg) tea.Cmd { return func() tea.Msg { return <-events } }

func (m *Model) add(line string) {
	if len(m.transcript) > 0 && strings.HasPrefix(line, "assistant: ") && strings.HasPrefix(m.transcript[len(m.transcript)-1], "assistant: ") {
		m.transcript[len(m.transcript)-1] += strings.TrimPrefix(line, "assistant: ")
		m.trimTranscript()
		return
	}
	m.transcript = append(m.transcript, line)
	m.trimTranscript()
}

func (m *Model) trimTranscript() {
	if len(m.transcript) > maxTranscriptLines {
		m.transcript = m.transcript[len(m.transcript)-maxTranscriptLines:]
	}
	bytes := len(m.transcript) - 1
	for _, line := range m.transcript {
		bytes += len(line)
	}
	for bytes > maxTranscriptBytes && len(m.transcript) > 0 {
		excess := bytes - maxTranscriptBytes
		if len(m.transcript[0]) <= excess {
			bytes -= len(m.transcript[0]) + 1
			m.transcript = m.transcript[1:]
			continue
		}
		m.transcript[0] = utf8Suffix(m.transcript[0], len(m.transcript[0])-excess)
		break
	}
}

func utf8Suffix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	start := len(value) - limit
	for start < len(value) && value[start]&0xc0 == 0x80 {
		start++
	}
	return value[start:]
}

func (m *Model) canRetry() bool {
	return !m.terminal && (m.lastRequest != nil || m.runID != "")
}

func (m *Model) header() string {
	content := strings.Join([]string{
		"agent-platform  " + sanitize(m.status),
		"url: " + sanitize(m.client.BaseURL),
		"thread: " + sanitize(m.threadID),
		"run: " + sanitize(m.runID),
		"journey: " + sanitize(m.journeyID),
	}, "\n")
	return wrap(content, max(1, m.viewport.Width()))
}

func (m *Model) layout() {
	if m.screenHeight > 0 {
		m.viewport.SetHeight(max(1, m.screenHeight-strings.Count(m.header(), "\n")-3))
	}
}

func wrap(value string, width int) string {
	var output strings.Builder
	for _, line := range strings.Split(value, "\n") {
		for len(line) > 0 {
			end := 0
			for end < len(line) && end < width {
				_, size := utf8.DecodeRuneInString(line[end:])
				end += size
			}
			output.WriteString(line[:end])
			line = line[end:]
			if line != "" {
				output.WriteByte('\n')
			}
		}
		output.WriteByte('\n')
	}
	return strings.TrimSuffix(output.String(), "\n")
}

func (m *Model) refresh() {
	wasAtBottom := m.viewport.AtBottom()
	content := append([]string{}, m.transcript...)
	if m.pending != nil {
		if m.pending.Kind == "approval" {
			content = append(content, "approval required: "+sanitize(m.pending.Call.Name), "arguments: "+sanitize(string(m.pending.Call.Arguments)), "type approve or deny")
		} else if m.questionIndex < len(m.pending.Questions) {
			question := m.pending.Questions[m.questionIndex]
			content = append(content, sanitize(question.Header)+": "+sanitize(question.Question))
			for _, option := range question.Options {
				content = append(content, "  "+sanitize(option.Label)+" — "+sanitize(option.Description))
			}
		}
	}
	m.viewport.SetContent(strings.Join(content, "\n"))
	if !wasAtBottom {
		return
	}
	m.viewport.GotoBottom()
}

func sanitize(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, value)
}

func newID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return "tui_" + hex.EncodeToString(bytes[:])
}
