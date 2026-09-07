# Architecture

Ooh is a single Go binary with an embedded React application. It uses the installed `git` executable for repository operations and a per-repository SQLite database for coordination metadata.

## Data and ownership

| Item | Location | Purpose |
| --- | --- | --- |
| Task metadata | `<common-git-dir>/ooh/state.db` | Tasks, paths, checkpoints, comments, candidates, archives |
| Checkpoint protection | `refs/ooh/checkpoints/<id>` | Keeps immutable checkpoint commits reachable |
| Undo protection | `refs/ooh/undo/<integration-id>` | Keeps the pre-integration base commit reachable |
| Path clones | OS user data directory under `ooh/workspaces/<repo-id>` | Gives every active variation its own working tree, branch, and Git directory |
| Artifacts and logs | `<common-git-dir>/ooh/` | Private local evidence and diagnostics |

No Ooh-specific data is required in the working tree. The optional project Agent Skill is installed at `.agents/skills/ooh` only after confirmation.

## Core invariants

1. A path owns one independent local clone and branch. Local clone objects are copied without hardlinks and no source remote is retained.
2. A checkpoint points to one commit and is never rewritten. Later feedback produces a new checkpoint.
3. All non-ignored work is staged when a checkpoint is created, so the frozen commit matches the visible workspace.
4. Checkpointing imports the clone commit into protected source-repository refs; selection then removes sibling clones while retaining those imported refs.
5. Integration happens from an isolated worktree and produces one squash commit based on the current base tip.
6. Applying a candidate requires its exact commit ID, an unchanged base ref, and a clean checked-out base worktree.
7. Undo requires the integrated commit to remain the clean base tip.

## Packages

- `internal/gitexec` is the narrow Git command boundary and porcelain parser.
- `internal/store` owns schema migration and SQLite queries.
- `internal/service` enforces state transitions, locking, retention, and integration safety.
- `internal/server` exposes the authenticated loopback API and embedded dashboard.
- `internal/cli` exposes human and stable JSON command surfaces.
- `internal/agentskill` previews and installs the embedded portable skill.

The service layer is shared by the CLI and HTTP server; the web application does not implement Git behavior itself.

## Concurrency

SQLite uses WAL mode with foreign keys and a single connection. Multi-step Git/state changes also acquire `<common-git-dir>/ooh/operation.lock`. The lock records a process ID and can recover when its owning process no longer exists.

Workspace status is polled every two seconds while the dashboard server runs. The browser refreshes task data periodically, so edits made by agents appear without a manual reload.

## Local processes

The dashboard detects installed Codex and Claude Code executables and launches them inside the selected path clone. Provider session IDs, state, and summaries are persisted in SQLite; raw run output remains in private files under `.git/ooh/runs/`. A successful run creates a ready checkpoint automatically. Active runs are marked interrupted if Ooh stops, and follow-up instructions resume the provider session when supported.

Preview commands require one explicit confirmation per repository. Preview processes and browser terminals are ephemeral children of the Ooh server and stop with it. Terminal WebSocket access uses a short-lived, single-use ticket in addition to the dashboard session token.

## Integration sequence

```text
selected checkpoint
        │
        ▼
isolated candidate worktree ── squash merge ──► conflict workspace (if needed)
        │
        ▼
single candidate commit ── final diff review ── exact commit confirmation
        │
        ▼
base cleanliness + tip preflight ── archive paths ── fast-forward base
        │
        ▼
undo ref + integration record
```

If the base branch advances, a new candidate must be prepared. Ooh does not silently rebase a reviewed candidate.
