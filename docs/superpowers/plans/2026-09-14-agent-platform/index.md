# Agent Platform Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `github.com/swiftdiaries/az-agent-platform-go`, a product-agnostic, PostgreSQL-backed Go service with durable hub-to-journey execution, AG-UI ingress/replay, provider/tool execution (MAF provisionally), safe human waits, and conservative effect recovery.

**Architecture:** Chat owns HTTP/AG-UI, authentication, external identity, and snapshot rendering from Agent records. Agent owns the authoritative command/event journal, routing, journey sessions, ownership, safe-point inbox, human interactions, and execution. MAF is the provisional internal execution kernel, subject to the spec’s product-slice gate; PostgreSQL is lifecycle truth, and Java remains a configurable MCP peer.

**Tech Stack:** Go 1.26.0; Microsoft Agent Framework Go pseudo-version `v0.0.0-20260914050536-4569dba84f21`; MCP Go SDK v1.7.0; AG-UI community Go SDK `v0.0.0-20260312103001-8e7ab1df34c8`; pgx v5; PostgreSQL 16; OpenTelemetry Go v1.46.0; Docker/Compose; Kubernetes manifests.

**Spec:** [`docs/superpowers/specs/2026-09-14-hub-and-spoke-agent-platform-design.md`](../../specs/2026-09-14-hub-and-spoke-agent-platform-design.md)

## Global Constraints

- The destination is a standalone project named `az-agent-platform-go`; the name does not imply Azure hosting or an Azure model provider.
- The module path is `github.com/swiftdiaries/az-agent-platform-go`; remote repository creation is outside this plan.
- Run every implementation command from the destination project root. All paths in this package are relative and portable.
- Preserve the Chat/Agent ownership boundary and the narrow `platform.Platform` seam in [`interfaces.md`](interfaces.md).
- Keep the platform schema free of vacation, shift, employee, balance, date, leave-approval, and other product fields.
- PostgreSQL is authoritative for stable Agent commands, events, run state, interactions and provider history. Framework sessions remain private and reconstructable; Chat renders snapshots from Agent records.
- At most one active specialist and one active run exist per conversation in V1.
- Raw credentials never enter PostgreSQL, definitions, transcripts, events, logs, or traces; the carrier is request/run-memory only.
- An accepted receipt means the command committed. An included event means a provider-boundary drain directly observed inclusion.
- Serialize command admission with terminal transition. A committed pending command must have live-owner delivery or a scheduled follow-up.
- Lost ownership interrupts the run; no replica automatically continues or replays it.
- An ambiguous effectful MCP call stops execution with operation outcome `outcome_unknown`; interrupted run state remains distinct and it is never automatically retried or returned to model autocall.
- Clarification supplies information only. Approval is one-time and bound to exact call, canonical arguments, principal, context, and definition digest.
- An empty or failed tool allowlist fails closed. Old compiled bundles remain available while referenced.
- Production telemetry excludes prompts, user text, skill bodies, MCP arguments/results, auth material, and raw business identifiers.
- Missing real model, auth, Java MCP, or cluster configuration is `BLOCKED`, never a passing integration result.
- Automatic recovery after a crash during a model or tool call, simultaneous specialists, workflow graphs, dynamic agents, runtime journey editing, long-term memory, and plugin marketplaces are outside V1.
- Use test-first steps, stage only task-owned paths, run `git diff --check` and `git diff --cached --check`, and obtain an independent review before integration.

## Delivery

The user requested this reduced plan on 2026-09-14. Six deliverables replace the incomplete 40-task draft. These are implementation scopes with acceptance criteria, not prewritten production code. Implement and independently review one deliverable at a time; expand only the next deliverable against the actual code.

| Deliverable | Depends on | Result | Status / owner / commit / checks / review |
|---|---|---|---|
| 1 | — | One authenticated, configured journey through AG-UI and MCP | planned / unassigned / — / — / — |
| 2 | 1 | Durable conversations, pinned sessions and reconnect | planned / unassigned / — / — / — |
| 3 | 2 | Any-replica ownership, steering and interruption | planned / unassigned / — / — / — |
| 4 | 3 | Durable clarification, exact approval and safe tool outcomes | planned / unassigned / — / — / — |
| 5 | 4 | Retained definitions, bounded skills and second journey | planned / unassigned / — / — / — |
| 6 | 5 | Kubernetes, Java/model integration and full acceptance | planned / unassigned / — / — / — |

The first deliverables are testable development increments, not production releases. All six and the complete acceptance suite are required for V1.

- [tasks.md](tasks.md): implementation scopes, verification commands and mapping of all twelve spec acceptance cases.
- [interfaces.md](interfaces.md): the stable module boundary and transaction invariants.
- [coordination.md](coordination.md): ownership, review and integration rules for the table above.
- [session-handoffs.md](session-handoffs.md): one reusable handoff.
- [source-evidence.md](source-evidence.md): pinned dependency evidence and preserved spike references.
- [review.md](review.md): outstanding implementation checks from the earlier review.

## Completion

Deliverable 6 records the exact Git revision, PostgreSQL/dependency versions, deterministic results, real-environment results or explicit `BLOCKED` entries, Kubernetes evidence, privacy checks and independent reviews. Missing required external evidence leaves V1 incomplete. This turn revises planning documents; it does not start implementation or publish a repository.
