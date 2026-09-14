package main

import (
	"fmt"
	"os"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

func main() {
	path := os.Getenv("AGENT_PLATFORM_CONFIG")
	if path == "" {
		path = "configs/journeys.yaml"
	}
	if _, err := definitions.Load(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "configuration valid; real OAuth, Foundry model, and Java MCP startup wiring is completed in Task 6")
}
