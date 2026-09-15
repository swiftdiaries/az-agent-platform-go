package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/swiftdiaries/az-agent-platform-go/internal/devtui"
)

const (
	envURL   = "AGENT_PLATFORM_URL"
	envToken = "KEYCLOAK_ACCESS_TOKEN"
)

type options struct {
	baseURL   string
	token     string
	journeyID string
	threadID  string
	runID     string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags(args []string, lookup func(string) string) (options, error) {
	baseURL := "http://localhost:8080"
	if lookup != nil {
		if value := strings.TrimSpace(lookup(envURL)); value != "" {
			baseURL = value
		}
	}

	flags := flag.NewFlagSet("agent-platform-tui", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var result options
	flags.StringVar(&result.baseURL, "url", baseURL, "agent platform URL")
	flags.StringVar(&result.journeyID, "journey", "", "journey ID")
	flags.StringVar(&result.threadID, "thread", "", "thread ID")
	flags.StringVar(&result.runID, "run", "", "run ID to resume")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if result.runID != "" && result.threadID == "" {
		return options{}, errors.New("--run requires --thread")
	}
	if strings.TrimSpace(result.baseURL) == "" {
		return options{}, errors.New("--url cannot be empty")
	}
	result.journeyID = strings.TrimSpace(result.journeyID)
	result.threadID = strings.TrimSpace(result.threadID)
	result.runID = strings.TrimSpace(result.runID)
	return result, nil
}

func run(args []string, lookup func(string) string, stdin, stdout *os.File) error {
	return runContext(context.Background(), args, lookup, stdin, stdout)
}

func runContext(ctx context.Context, args []string, lookup func(string) string, stdin, stdout *os.File) error {
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return errors.New("agent-platform-tui requires an interactive TTY")
	}
	result, err := parseFlags(args, lookup)
	if err != nil {
		return err
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	result.token = strings.TrimSpace(lookup(envToken))
	if result.token == "" {
		return errors.New("KEYCLOAK_ACCESS_TOKEN is required")
	}

	model := altScreenModel{Model: devtui.NewModel(devtui.Client{
		BaseURL:    result.baseURL,
		Token:      result.token,
		HTTPClient: http.DefaultClient,
	}, result.threadID, result.runID, result.journeyID)}
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err = program.Run()
	return err
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type altScreenModel struct{ tea.Model }

func (m altScreenModel) Init() tea.Cmd { return m.Model.Init() }

func (m altScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, command := m.Model.Update(msg)
	return altScreenModel{Model: model}, command
}

func (m altScreenModel) View() tea.View {
	view := m.Model.View()
	view.AltScreen = true
	return view
}
