// Package devtui is the local terminal client for the public AG-UI endpoint.
package devtui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxSSELine  = 256 << 10
	maxSSEEvent = 256 << 10
)

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type Request struct {
	ThreadID  string
	RunID     string
	JourneyID string
	Text      string
	Reply     *InteractionReply
}

type InteractionReply struct {
	InteractionID string `json:"interactionId"`
	Kind          string `json:"kind"`
	Answer        any    `json:"answer"`
}

type Event struct {
	Type string
	Data json.RawMessage
}

type chatRequest struct {
	ThreadID       string         `json:"threadId"`
	RunID          string         `json:"runId"`
	Messages       []message      `json:"messages"`
	Tools          []any          `json:"tools"`
	Context        []any          `json:"context"`
	State          map[string]any `json:"state"`
	ForwardedProps forwardedProps `json:"forwardedProps"`
}

type message struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type forwardedProps struct {
	JourneyID        string            `json:"journeyId,omitempty"`
	InteractionReply *InteractionReply `json:"interactionReply,omitempty"`
}

func (c Client) Stream(ctx context.Context, request Request, receive func(Event)) error {
	messages := []message{{ID: request.RunID + ":user", Role: "user", Content: request.Text}}
	if request.Reply != nil {
		messages = []message{}
	}
	body, err := json.Marshal(chatRequest{
		ThreadID: request.ThreadID, RunID: request.RunID,
		Messages: messages,
		Tools:    []any{}, Context: []any{}, State: map[string]any{},
		ForwardedProps: forwardedProps{JourneyID: request.JourneyID, InteractionReply: request.Reply},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, receive)
}

func (c Client) Resume(ctx context.Context, threadID, runID string, receive func(Event)) error {
	endpoint, err := url.Parse(c.endpoint())
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("threadId", threadID)
	query.Set("runId", runID)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	return c.do(req, receive)
}

func (c Client) endpoint() string { return strings.TrimRight(c.BaseURL, "/") }

func (c Client) do(request *http.Request, receive func(Event)) error {
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	request.Header.Set("Accept", "text/event-stream")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("chat request failed: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return parseSSE(response.Body, receive)
}

func parseSSE(body io.Reader, receive func(Event)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4<<10), maxSSELine)
	var event Event
	emit := func() {
		if len(event.Data) == 0 {
			return
		}
		if event.Type == "" {
			var typed struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(event.Data, &typed)
			event.Type = typed.Type
		}
		if receive != nil {
			receive(event)
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			emit()
			event = Event{}
		case strings.HasPrefix(line, "event:"):
			event.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if len(event.Data)+len(data)+1 > maxSSEEvent {
				return fmt.Errorf("SSE event exceeds %d bytes", maxSSEEvent)
			}
			if len(event.Data) > 0 {
				event.Data = append(event.Data, '\n')
			}
			event.Data = append(event.Data, data...)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	emit()
	return nil
}
