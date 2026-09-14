# Execution coordination

Use the delivery table in [index.md](index.md) as the only task tracker. No custom scheduler, JSON manifest or report archive is required.

1. The coordinator assigns one ready deliverable and records its owner, exact base commit, worktree and named writable paths in the table. Dependencies must have integrated, reviewed evidence.
2. Authors implement in isolated worktrees. Parallel work, when useful, must have disjoint writable paths; the coordinator owns integration and shared-file scheduling.
3. Use the least costly capable author; raise capability for durability/security decisions. The root coordinates and reviews; implementation belongs to assigned workers.
4. Write the focused behavioral test first, observe its failure, implement, and run the task's checks. Use real isolated PostgreSQL for transaction tests; absent external prerequisites are `BLOCKED`, not passing evidence.
5. Record the submitted commit, exact check commands/results and independent review in the table or a linked concise report. Review the exact submitted revision; cover both Chat and Agent for boundary changes.
6. Integrate one reviewed change at a time, stage only assigned files, run `git diff --check` and `git diff --cached --check`, and run affected checks on the integrated revision before releasing dependents.
7. Resume from the recorded commit and next unchecked step. Stop a stale worker before replacing it; preserve its changes, use a new worktree, and review any revised commit again. A stale branch is never evidence of completion.

Keep independent review and verification. Split a deliverable only when its implementation exposes a useful independent review boundary, not to meet a task-count quota.
