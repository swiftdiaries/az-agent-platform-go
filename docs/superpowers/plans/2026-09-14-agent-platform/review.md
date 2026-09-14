# Review carry-forward

The earlier review covered an incomplete 40-task draft; it did not approve execution. The user requested the six-deliverable simplification on 2026-09-14. This note retains its substantive checks; removing draft code does not prove the implementation correct.

| Required check | New task |
|---|---|
| Serialize admission/finish in both commit orders; no stranded live-owner command | 3 |
| Claim an explicit pending run; expired owner is interrupted, never taken over; database clock after lock | 3 |
| Atomic history/call/interaction/wait/owner-release checkpoint | 4 |
| Atomic reply/decision and continuation; crash injection and payload idempotency | 4 |
| Exact question cardinality and server-stored approval binding with lossless numbers | 4 |
| Direct provider-boundary inclusion evidence, never context-build acknowledgement | 3 |
| Separate open-wait uniqueness from live execution uniqueness | 3, 4 |
| Consistent snapshot/watermark, cursor edge cases and lost notifications | 2 |
| Separate startup symbolic validation from authenticated MCP binding | 1, 5 |
| No retry multiplication, partial-stream duplication or exactly-once external-effect claim | 4 |
| No external calls inside database transaction callbacks | 4 |
| Actual AG-UI, Java, model and Kubernetes evidence with honest missing-prerequisite status | 6 |

The reduced plan intentionally replaces premature signatures and production-code snippets with deliverable-level behavioral criteria. Expand the next assigned task against the actual code and review it independently. Dependency/module pins remain evidence to verify, not a guarantee that all integrations already work.
