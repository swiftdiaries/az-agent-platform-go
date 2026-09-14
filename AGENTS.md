# Working Agreement

## Context7

Use Context7 MCP to fetch current documentation whenever work concerns a library, framework, SDK, API, CLI tool, or cloud service. Prefer Context7 to web search for those references. Do not use it for refactoring, scripts written from scratch, business-logic debugging, code review, or general programming concepts.

Start with `resolve-library-id` using the library name and the documentation topic, unless the task provides an exact `/org/project` ID. Choose the best result by exact name, relevance, reputable source quality, useful example coverage, and version match when a version is named. Then use `query-docs` for one concept at a time. If a request spans distinct concepts, query each separately unless their interaction is the question.

## Model routing

The root orchestrator selects the least costly model and reasoning effort that can meet the task's quality bar. Base the choice on complexity, ambiguity, risk, reversibility, required context, tool burden, nuance, taste, and the strength of available verification. Increase capability for subtle architecture, security, difficult diagnosis, cross-system synthesis, ambiguous requirements, or consequential decisions.

Delegate routine, bounded, independently verifiable work only when the savings exceed coordination cost; provide scope, constraints, required output, and verification, then review and integrate the result. Periodically review archive spend and aim for less than 40% of total model cost in the most expensive model.

## Execution boundaries

The root orchestrator is orchestration-only: it briefs workers, reviews their work, and coordinates integration; implementation belongs to workers in their assigned worktrees. Parallel implementation uses separate worktrees. Choose review checkpoints from actual task dependencies and risk; do not impose an arbitrary task-count review cadence. This seed contains no authorization gate beyond the approved design and the user-selected fresh implementation sessions.
