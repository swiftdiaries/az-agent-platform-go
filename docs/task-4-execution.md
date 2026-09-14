# Task 4 implementation candidate

Base: `6d2e684eb7b81adc0236e8ec301ab1a0a1f20a86` (reviewed Tasks 1–3). Scope is Task 4 only. Independent specification and quality reviews are pending.

The selected branch is the approved product-owned resume-in-place fallback. The pinned MAF graph's single-request round trip passes, but its two-outstanding-request resume calls the provider once with only one result and then again, producing duplicate output. Both findings are executable in `internal/runtime/framework_contract_test.go`; the negative gate asserts the observed limitation and therefore passes as regression evidence. Production keeps the existing MAF single-provider-turn seam and durable PostgreSQL authority.

Real-fixture evidence: PostgreSQL 16.13, Go 1.26.0, stateful MCP Go SDK v1.7.0. The suite starts an isolated database per test and fails rather than skips when PostgreSQL/Docker is unavailable. The process test ends one OS process after wait commit and a second after reply-admission commit, then explicitly resumes the one admitted continuation using fresh ingress.

Validation commands:

```sh
go test ./... -count=1
go test -race ./internal/... ./integration -run 'TestHITL|TestReply|TestToolCall|TestApproval|TestBusinessError|TestOwnership|TestSteering|TestAdmissionFinish' -count=1
go vet ./...
git diff --check
```

The full scenario mapping and exact framework failure are in `docs/superpowers/plans/2026-09-14-agent-platform/source-evidence.md`. Task 5 and Task 6 remain unimplemented here. No live Java/provider/cluster acceptance is claimed.

Final checks: full suite PASS (integration 18.986s; runtime 1.157s), focused race selection PASS, `go vet ./...` PASS, and `git diff --check` PASS. The implementation commit remains a review candidate, not an independent-review approval.
