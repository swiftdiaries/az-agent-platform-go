# Implementation handoff

Use one brief for an author or independent reviewer. The coordinator fills in the scope from [index.md](index.md); no separate handoff database is needed.

```text
Role: author or independent reviewer
Deliverable and acceptance cases: copy from tasks.md
Worktree, branch and exact base/submitted commit: record actual values
Writable paths: list the assigned files; reviewers do not edit source
Read: spec, index.md, interfaces.md, assigned tasks.md section and source-evidence.md
Constraints: preserve the spec's security, durability and recovery guarantees
Verify: run the assigned commands; report exact results and missing prerequisites
Return: commit, changed paths, check results, review findings and next unchecked step
```

Follow [coordination.md](coordination.md). An author revision needs review at its new commit; a replacement session checks the recorded branch and working tree before proceeding.
