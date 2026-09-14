# Hub-and-Spoke Agent Platform: Work Ledger

Status: design approved; qualified MAF durability spike reproduced and independently reviewed; six-deliverable implementation plan prepared

Go framework source revision: `4569dba84f21797ae57053c26603f54621785b26`

Authority: user approved the design and requested spec/task simplification on 2026-09-14. This work updates planning documents; implementation has not started.

## Completed research

- Go Microsoft Agent Framework at `4569dba84f21797ae57053c26603f54621785b26`: verified session serialization, message injection and safe-point limits, agent-as-tool behavior, MCP filtering responsibility, skills gap, OpenTelemetry, and AG-UI adapter behavior.
- Codex at clean `main` `5b1d6560181680f95cde95c14ed042acc02248ed`: compared durable queued communication, thread/event persistence, tool/skill loading, recovery boundaries, and application-server seams.
- Pi at verified local and remote `main` `ceea48f5d5d12fd7915dfefba2835ccd55f23bb9`: corrected an earlier stale reading and verified the implemented durable harness, admission/drive split, correction queues, lane recovery, and snapshot-plus-event UI seam.
- Kagent at verified local and remote `main` `2d843e3732c308d7728c4c7daa1708a96277f159`: compared immutable compiled revisions, prompt/tool/skill resolution, version pinning, and Kubernetes runtime boundaries.
- Substrate at the user-referenced clean local `main` `672533541dbfcd29084e4de2475267088bda3651`; configured-origin `main` was separately verified as divergent `4a4c59ad7274428f5aa57a5ffda80ebfb6bc9802`: extracted the payload-before-manifest publication invariant and kept sandbox/process snapshots outside V1.
- Drafted and refined the product-neutral module boundary, journey lifecycle, correction contract, persistence ownership, AG-UI mapping, MCP boundary, observability, deployment model, recovery limits, and first vertical slice.
- Completed two independent reviews covering contradictions, feasibility, scope, execution authority, correction acknowledgement, run-closing races, and recovery claims; incorporated the actionable findings into the refined draft.
- Compared actual Go MAF, Codex, and Pi retry/error behavior and added a proposed single-owner retry taxonomy without treating reference defaults as approved policy.
- Verified AG-UI/Go MAF human-input seams and recorded PostgreSQL-backed clarification and exact-action approval waits as application-owned durability.
- Completed the isolated MAF compatibility spike on `codex/spike-maf-hitl` at base `4569dba84f21797ae57053c26603f54621785b26`. Eight behavioral scenarios passed against fresh PostgreSQL 16 using separate OS processes, real MAF sessions/autocall, and streamable-HTTP MCP; two additional entrypoints are subprocess helpers.
- Independently reproduced and reviewed the spike. The final credential assertion proved neither pre-wait nor resumed credentials entered durable state. Preserved evidence: `spikes/maf_hitl/README.md`, `/tmp/agent-platform-maf-spike-review.md`, `/private/tmp/agent-framework-go-maf-hitl-spike-final.log` (SHA-256 `78cb94c31bcaa45e6d45111eb2c57ff7007d364ada856649fe69eb70d054997f`), and `/private/tmp/agent-framework-go-maf-hitl-spike-independent.log` (SHA-256 `8eea996b11b1c86c8b3cc6a76a21ba16def58fbd671746e9181d6467eb4c5a73`).
- Qualified approach B for native `toolautocall` approval restoration, typed clarification restoration, fresh-credential handoff, and an application-level uncertain-write stop. The spike changed no MAF framework source; its 201 non-test adapter lines are not a production glue estimate.

## Confirmed requirements

1. The product is a general, product-agnostic agent platform; vacation and shift-management flows are examples rather than framework concepts.
2. A journey retains its specialist context and permits one active execution at a time.
3. V1 supports idle journey resume and reconnect to a live run; process-death recovery of active execution is outside the required scope.
4. The hub routes automatically and also accepts an explicit specialist target.
5. A spoke receives the task, selected hub context, and its own journey history.
6. Agent definitions are loaded and versioned at startup.
7. Existing journeys remain pinned to their original definition bundle and context.
8. Skills use progressive-disclosure `SKILL.md` resources.
9. PostgreSQL owns V1 durable state; object-store snapshots are reserved for possible future asynchronous workflow recovery.
10. The service runs as interchangeable Kubernetes replicas, and any replica may accept ingress.
11. The UI sends current-user identity headers; the Go service forwards only the scoped identity needed by authorized Java MCP calls.
12. Clarification questions and exact action approvals share a first-class structured interaction mechanism; answering a question does not grant action approval.
13. A deliberate human wait persists across pod replacement, releases execution ownership, and resumes the pinned journey on any replica without replaying completed tools.

## Current plan

- [Six deliverables](../plans/2026-09-14-agent-platform/index.md) replace the incomplete 40-task draft and its custom scheduling machinery. The plan retains all twelve acceptance cases and thirteen confirmed requirements.
- Internal interfaces grow from actual consumers; lifecycle checks stay in their owning transactions; definitions load immutably at startup; Chat renders snapshots from Agent records.
- Next implementation work starts with the first deliverable in the standalone destination, through the user's selected fresh sessions. No production code, commits, tickets or publication are part of this trim.
- The product slice still must prove atomic reply-plus-continuation, timeout and MCP isError classification, actual AG-UI/Kubernetes/Java/model integration and definition lineage. Planning is not passing evidence.

## Routing

- Root orchestrator: owns the user interview, decisions, spike coordination, final synthesis, and approval boundary.
- Evidence and independent-review units: routed to `gpt-5.6-sol` at high reasoning effort for bounded source verification and architectural challenge.
- Spike implementation: `/root/maf_hitl_spike`, `gpt-5.6-sol`, high reasoning effort.
- Spike review: `/root/maf_spike_review`, `gpt-5.6-sol`, high reasoning effort.
- Main spec author: owns spec, `CONTEXT.md`, and ledger synthesis.
- The completed spike was the only implementation authorized by this ledger. Full platform implementation, commits, ticket creation, and publication still require their own scope or approval.
