package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/swiftdiaries/az-agent-platform-go/internal/devtui"
)

const (
	envURL   = "AGENT_PLATFORM_URL"
	envToken = "KEYCLOAK_ACCESS_TOKEN"

	readinessTimeout = 5 * time.Second
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
	if wantsHelp(args) {
		printUsage(stdout)
		return nil
	}
	result, err := parseFlags(args, lookup)
	if err != nil {
		return err
	}
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return errors.New("agent-platform-tui requires an interactive TTY")
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	result.token = strings.TrimSpace(lookup(envToken))
	if result.token == "" {
		return errors.New("KEYCLOAK_ACCESS_TOKEN is required")
	}
	if err := checkReadiness(ctx, result.baseURL, nil); err != nil {
		return err
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
	return file != nil && term.IsTerminal(file.Fd())
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: go run ./cmd/agent-platform-tui [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --url URL       agent platform URL (default: AGENT_PLATFORM_URL or http://localhost:8080)")
	fmt.Fprintln(w, "  --journey ID    journey ID")
	fmt.Fprintln(w, "  --thread ID     existing thread for a new run or resume")
	fmt.Fprintln(w, "  --run ID        run ID to resume (requires --thread)")
	fmt.Fprintln(w, "Authentication: set KEYCLOAK_ACCESS_TOKEN in the environment")
}

func checkReadiness(ctx context.Context, baseURL string, client *http.Client) error {
	endpoint, err := readinessEndpoint(baseURL)
	if err != nil {
		return errors.New("invalid agent platform URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid agent platform URL")
	}
	if client == nil {
		client = &http.Client{}
	} else {
		copy := *client
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 || client.Timeout > readinessTimeout {
		client.Timeout = readinessTimeout
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("agent platform readiness check failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("agent platform readiness check returned HTTP %d", response.StatusCode)
	}
	return nil
}

func readinessEndpoint(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/readyz"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
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
