package main

import (
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

func TestAltScreenModelSetsViewMode(t *testing.T) {
	model := altScreenModel{Model: testViewModel{}}
	if !model.View().AltScreen {
		t.Fatal("expected alternate screen")
	}
}

type testViewModel struct{}

func (testViewModel) Init() tea.Cmd                       { return nil }
func (testViewModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return testViewModel{}, nil }
func (testViewModel) View() tea.View                      { return tea.NewView("test") }
