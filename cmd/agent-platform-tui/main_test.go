package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestParseFlagsDefaultsAndResumeRules(t *testing.T) {
	lookup := func(name string) string {
		if name == envURL {
			return "http://example.test/"
		}
		return ""
	}

	options, err := parseFlags([]string{"--thread", "thread-1", "--run", "run-1", "--journey", "vacation-planner"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if options.baseURL != "http://example.test/" || options.threadID != "thread-1" || options.runID != "run-1" || options.journeyID != "vacation-planner" {
		t.Fatalf("unexpected options: %+v", options)
	}

	if _, err := parseFlags([]string{"--run", "run-1"}, lookup); err == nil || !strings.Contains(err.Error(), "--thread") {
		t.Fatalf("expected --thread validation, got %v", err)
	}
}

func TestRunRejectsNonTTYBeforeReadingToken(t *testing.T) {
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdinWriter.Close()
	stdout, stdoutReader, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer stdoutReader.Close()

	lookup := func(name string) string {
		if name == envToken {
			t.Fatal("token read before TTY check")
		}
		return ""
	}
	if err := run([]string{}, lookup, stdin, stdout); err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("expected TTY error, got %v", err)
	}
}

func TestIsTerminalRejectsDevNull(t *testing.T) {
	file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Fatal("/dev/null must not be treated as an interactive terminal")
	}
}

func TestRunHelpSkipsTTYAndTokenChecks(t *testing.T) {
	stdin, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	reader, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--help"}, func(string) string {
		t.Fatal("environment read for --help")
		return ""
	}, stdin, stdout); err != nil {
		t.Fatal(err)
	}
	stdout.Close()
	defer reader.Close()
	usage, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(usage), "--journey") || !strings.Contains(string(usage), "--run") || !strings.Contains(string(usage), "/retry") {
		t.Fatalf("incomplete usage: %s", usage)
	}
}

func TestCheckReadinessSanitizesErrorsAndDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("Authorization") != "" {
			t.Fatal("readiness request must not include bearer token")
		}
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Status:     "307 Temporary Redirect",
			Body:       io.NopCloser(strings.NewReader("secret response body")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	err := checkReadiness(t.Context(), "http://example.test", client)
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("unexpected readiness error: %v", err)
	}
	if requests != 1 {
		t.Fatalf("redirect was followed: %d requests", requests)
	}
}

func TestAltScreenModelSetsViewMode(t *testing.T) {
	model := altScreenModel{Model: testViewModel{}}
	if !model.View().AltScreen {
		t.Fatal("expected alternate screen")
	}
}

type testViewModel struct{}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (testViewModel) Init() tea.Cmd                       { return nil }
func (testViewModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return testViewModel{}, nil }
func (testViewModel) View() tea.View                      { return tea.NewView("test") }
