# Ooh

**Parallel agent work that people can actually review.**

Ooh is a local, open-source Git tool for letting several coding agents explore a task at the same time. Every approach gets a separate workspace. When an approach is ready, Ooh freezes it into a checkpoint and presents the result in a calm, summary-first web review.

It borrows the parallel-work spirit of [GitButler](https://gitbutler.com) and the focused local review experience of [Difit](https://github.com/yoshiko-pg/difit), while keeping the underlying model intentionally small: isolated local clones, local SQLite metadata, and one portable binary.

> Ooh is an early alpha. Try it on repositories you can recover, and please report rough edges.

## What it feels like

1. Click **New task** and describe the outcome in the setup modal.
2. Choose the number of variations, then select the coding agent and model for each one. Optional guidance can steer each approach.
3. Start the task. Ooh creates every isolated clone at the current HEAD and launches the selected agents.
4. Review frozen checkpoints in the local dashboard, with plain-language summaries and file diffs.
5. Choose a result. Ooh prepares one clean commit in isolation and shows the exact final diff.
6. Apply it to the base branch, with a safe undo record.

Ooh launches installed Codex or Claude Code CLIs using their existing local authentication. Ooh itself does not call a hosted model API, upload code, or require an Ooh account.

## Install

Prebuilt macOS and Linux binaries will be attached to each GitHub release.

```sh
brew install keyanfayaz/tap/ooh
```

Or build from source with Go 1.26+, Node.js 24+, and pnpm 10:

```sh
pnpm --dir web install
make build
./bin/ooh version
```

Ooh requires Git 2.31 or newer and a repository with at least one commit.

## Quick start

Run Ooh anywhere inside a Git repository:

```sh
ooh
```

The dashboard opens on `127.0.0.1` with a short-lived local session token. Click **New task** to configure the task in a modal. Choose the variation count, agent, model, and optional per-variation direction; Ooh then starts every selected agent in a separate clone and opens the editor workspace.

The workspace includes a file explorer, lightweight code editor, resumable agent output, optional application Preview, and a local terminal. Successful runs become ready checkpoints and open in a Review tab automatically.

The same flow is available from the command line:

```sh
ooh task create "Simplify sign-up" \
  --description "Make the first-run experience easier to understand"

# Run this from the printed workspace when it is ready for review:
ooh ready \
  --summary "Reduces sign-up to three clear steps" \
  --verification-status passed \
  --verification "pnpm test"

ooh
```

To create alternatives from the current path:

```sh
ooh path fork "Small change" "Fresh approach" \
  --summary "Compare a minimal edit with a simpler redesign"
```

## Agent setup

Ooh works with any agent that can edit a local folder and use Git. Install the bundled portable [Agent Skill](https://agentskills.io) into the project so supported agents understand checkpoints, review feedback, and safety boundaries:

```sh
ooh agent setup             # preview only
ooh agent setup --yes       # install into .agents/skills/ooh
```

The skill deliberately requires explicit user instruction before choosing, integrating, undoing, archiving, or deleting work.

## The small mental model

- **Task** — one outcome, based on one local branch.
- **Path** — one approach to the task, with its own local clone, Git directory, and branch. Objects are copied without hardlinks and the source remote is removed.
- **Checkpoint** — an immutable snapshot of all tracked and untracked, non-ignored work in a path.
- **Ready checkpoint** — a checkpoint with a human-readable summary, verification result, and optional risks or evidence.
- **Integration candidate** — one clean commit prepared in a separate worktree. The base branch does not move until the candidate is confirmed.

Agents can use Git normally inside managed workspaces. Ooh adds coordination and review; it does not invent a new version-control format.

## Useful commands

```text
ooh                                  Open the dashboard
ooh task create "Title"              Create a task and workspace
ooh task import <local-branch>       Adopt existing branch work
ooh task list                        List tasks
ooh task show [task-id]              Show task context and paths
ooh path fork "A" "B"                Create parallel approaches
ooh checkpoint create [name]        Save a recoverable snapshot
ooh ready --summary ...             Freeze a reviewable checkpoint
ooh feedback list --prompt          Format open feedback for an agent
ooh path select <path-or-cp-id>     Choose a result
ooh integrate prepare [task-id]     Prepare one isolated commit
ooh integrate show <candidate-id>   Inspect the exact candidate
ooh integrate apply <id> --confirm <commit-oid>
ooh undo <integration-id>           Safely restore the prior base tip
ooh doctor --bundle                 Export a reviewable diagnostic bundle
```

Every command supports `--json` with a versioned envelope for agent and script use.

## Safety and privacy

- The web server binds only to `127.0.0.1`, authenticates API requests with a random token, and sends a restrictive content-security policy.
- Repository metadata lives under the repository’s common Git directory at `.git/ooh/`.
- Managed path clones live in the user data directory, outside the source repository. Completed checkpoint commits are imported into the source repository only for review and integration.
- Checkpoints are protected by `refs/ooh/checkpoints/*`; Ooh never force-pushes or publishes them.
- Integration first creates and previews an isolated squash candidate. A dirty or advanced base branch stops the operation before task paths are archived.
- Undo is allowed only while the integrated commit is still the clean base tip.
- Archived path clones are removed immediately. Their imported branches and checkpoints are retained for 30 days by default, unless marked **keep**.
- Diagnostic bundles stay local and intentionally exclude source contents, diffs, environment variables, and Git remotes.

Read [docs/architecture.md](docs/architecture.md) for implementation details and [SECURITY.md](SECURITY.md) for reporting security issues.

## Development

```sh
pnpm --dir web install
make check
make test
make build
```

The Go tests include real-repository lifecycle coverage for isolated clones, automatic agent checkpoints, parallel paths, checkpoint diffs, integration preflight, apply, and undo. The dashboard is compiled into the Go binary.

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT © Ooh contributors
