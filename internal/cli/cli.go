package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/keyanfayaz/git-ooh/internal/agentskill"
	"github.com/keyanfayaz/git-ooh/internal/gitexec"
	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/server"
	"github.com/keyanfayaz/git-ooh/internal/service"
)

type Runner struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Cwd     string
	Version string
}

func New(version string) *Runner {
	cwd, _ := os.Getwd()
	return &Runner{Stdout: os.Stdout, Stderr: os.Stderr, Cwd: cwd, Version: version}
}

func (r *Runner) Execute(ctx context.Context, args []string) int {
	jsonMode := removeBoolFlag(&args, "--json")
	if len(args) == 0 {
		args = []string{"open"}
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		r.success(jsonMode, map[string]string{"version": r.Version}, "ooh "+r.Version)
		return 0
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.Stdout, usage)
		return 0
	}
	svc, err := service.Open(ctx, r.Cwd)
	if err != nil {
		return r.fail(jsonMode, err)
	}
	defer svc.Close()
	data, human, err := r.dispatch(ctx, svc, args, jsonMode)
	if err != nil {
		return r.fail(jsonMode, err)
	}
	if data != nil || human != "" {
		r.success(jsonMode, data, human)
	}
	return 0
}

func (r *Runner) dispatch(ctx context.Context, svc *service.Service, args []string, jsonMode bool) (any, string, error) {
	switch args[0] {
	case "open":
		opt, err := parseOptions(args[1:], "no-open")
		if err != nil {
			return nil, "", err
		}
		if url, ok := server.ExistingURL(ctx, svc.StateDir); ok {
			if !opt.Bool("no-open") {
				_ = server.OpenURL(url)
			}
			return map[string]string{"url": url}, "Ooh is already running at " + url, nil
		}
		srv, err := server.New(svc, r.Version)
		if err != nil {
			return nil, "", err
		}
		_, err = srv.Run(ctx, opt.Bool("no-open"), func(url string) {
			if jsonMode {
				r.success(true, map[string]string{"url": url}, "")
			} else {
				fmt.Fprintln(r.Stdout, "Ooh dashboard:", url)
				fmt.Fprintln(r.Stdout, "Press Ctrl+C to stop the local server.")
			}
		})
		return nil, "", err
	case "task":
		return r.task(ctx, svc, args[1:])
	case "path":
		return r.path(ctx, svc, args[1:])
	case "checkpoint":
		return r.checkpoint(ctx, svc, args[1:], false)
	case "ready":
		return r.checkpoint(ctx, svc, append([]string{"create"}, args[1:]...), true)
	case "feedback":
		return r.feedback(ctx, svc, args[1:])
	case "integrate":
		return r.integrate(ctx, svc, args[1:])
	case "undo":
		if len(args) != 2 {
			return nil, "", errors.New("usage: ooh undo <integration-id>")
		}
		result, err := svc.UndoIntegration(ctx, args[1])
		return result, "Integration undone. The selected task path is available again.", err
	case "archive":
		return r.archive(ctx, svc, args[1:])
	case "agent":
		return r.agent(svc, args[1:])
	case "doctor":
		return r.doctor(ctx, svc, args[1:])
	default:
		return nil, "", fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func (r *Runner) task(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("usage: ooh task <create|import|list|show|archive|restore>")
	}
	switch args[0] {
	case "create":
		opt, err := parseOptions(args[1:])
		if err != nil {
			return nil, "", err
		}
		if len(opt.Args) == 0 {
			return nil, "", errors.New("usage: ooh task create <title> [--description text] [--base branch]")
		}
		detail, err := svc.CreateTask(ctx, r.Cwd, strings.Join(opt.Args, " "), opt.One("description"), opt.One("base"))
		if err != nil {
			return nil, "", err
		}
		return detail, fmt.Sprintf("Task created: %s\nWorkspace: %s\nTask ID: %s", detail.Task.Title, detail.Paths[0].WorktreePath, detail.Task.ID), nil
	case "import":
		opt, err := parseOptions(args[1:])
		if err != nil {
			return nil, "", err
		}
		if len(opt.Args) != 1 {
			return nil, "", errors.New("usage: ooh task import <local-branch> [--title text] [--base branch]")
		}
		detail, err := svc.ImportTask(ctx, r.Cwd, opt.Args[0], opt.One("title"), opt.One("description"), opt.One("base"))
		if err != nil {
			return nil, "", err
		}
		return detail, fmt.Sprintf("Branch imported as task: %s\nWorkspace: %s\nTask ID: %s", detail.Task.Title, detail.Paths[0].WorktreePath, detail.Task.ID), nil
	case "list":
		tasks, err := svc.ListTasks(ctx)
		if err != nil {
			return nil, "", err
		}
		service.SortTasksByState(tasks)
		var b strings.Builder
		if len(tasks) == 0 {
			b.WriteString("No Ooh tasks yet. Create one with: ooh task create \"Task title\"")
		}
		for _, task := range tasks {
			fmt.Fprintf(&b, "%-20s  %-18s  %s\n", shortID(task.ID), task.State, task.Title)
		}
		return tasks, strings.TrimRight(b.String(), "\n"), nil
	case "show":
		opt, err := parseOptions(args[1:])
		if err != nil {
			return nil, "", err
		}
		taskID := ""
		if len(opt.Args) > 0 {
			taskID = opt.Args[0]
		} else {
			p, inferErr := svc.CurrentPath(ctx, r.Cwd, opt.One("path"))
			if inferErr != nil {
				return nil, "", inferErr
			}
			taskID = p.TaskID
		}
		detail, err := svc.TaskDetail(ctx, taskID)
		if err != nil {
			return nil, "", err
		}
		return detail, taskDetailText(detail), nil
	case "archive", "restore":
		if len(args) != 2 {
			return nil, "", fmt.Errorf("usage: ooh task %s <task-id>", args[0])
		}
		var detail model.TaskDetail
		var err error
		if args[0] == "archive" {
			detail, err = svc.ArchiveTask(ctx, args[1])
		} else {
			detail, err = svc.RestoreTask(ctx, args[1])
		}
		return detail, fmt.Sprintf("Task %s: %s", args[0]+"d", detail.Task.Title), err
	default:
		return nil, "", fmt.Errorf("unknown task command %q", args[0])
	}
}

func (r *Runner) path(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("usage: ooh path <fork|select|restore>")
	}
	switch args[0] {
	case "fork":
		opt, err := parseOptions(args[1:])
		if err != nil {
			return nil, "", err
		}
		p, err := svc.CurrentPath(ctx, r.Cwd, opt.One("path"))
		if err != nil {
			return nil, "", err
		}
		detail, err := svc.ForkPath(ctx, p, opt.Args, opt.One("summary"))
		if err != nil {
			return nil, "", err
		}
		var b strings.Builder
		b.WriteString("Paths are ready:\n")
		for _, path := range detail.Paths {
			fmt.Fprintf(&b, "- %s: %s\n", path.Name, path.WorktreePath)
		}
		return detail, strings.TrimRight(b.String(), "\n"), nil
	case "select":
		if len(args) != 2 {
			return nil, "", errors.New("usage: ooh path select <path-or-checkpoint-id>")
		}
		detail, err := svc.SelectResult(ctx, args[1])
		return detail, "Selected result for task: " + detail.Task.Title, err
	case "restore":
		if len(args) != 2 {
			return nil, "", errors.New("usage: ooh path restore <path-id>")
		}
		p, err := svc.RestorePath(ctx, args[1])
		return p, "Path restored at: " + p.WorktreePath, err
	default:
		return nil, "", fmt.Errorf("unknown path command %q", args[0])
	}
}

func (r *Runner) checkpoint(ctx context.Context, svc *service.Service, args []string, ready bool) (any, string, error) {
	if len(args) == 0 || args[0] != "create" {
		return nil, "", errors.New("usage: ooh checkpoint create [name] [options]")
	}
	opt, err := parseOptions(args[1:])
	if err != nil {
		return nil, "", err
	}
	p, err := svc.CurrentPath(ctx, r.Cwd, opt.One("path"))
	if err != nil {
		return nil, "", err
	}
	name := opt.One("name")
	if name == "" && len(opt.Args) > 0 {
		name = strings.Join(opt.Args, " ")
	}
	input := service.CheckpointInput{Name: name, Summary: opt.One("summary"), VerificationStatus: opt.One("verification-status"), VerificationNotes: opt.One("verification"), RiskNotes: opt.One("risk"), Ready: ready, ArtifactPaths: opt.Many("artifact")}
	cp, err := svc.CreateCheckpoint(ctx, p, input)
	if err != nil {
		return nil, "", err
	}
	label := "Checkpoint created"
	if ready {
		label = "Ready for review"
	}
	return cp, fmt.Sprintf("%s: %s\nCheckpoint ID: %s", label, cp.Name, cp.ID), nil
}

func (r *Runner) feedback(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("usage: ooh feedback <list|add|resolve|reopen>")
	}
	opt, err := parseOptions(args[1:], "prompt")
	if err != nil {
		return nil, "", err
	}
	checkpointID := opt.One("checkpoint")
	if checkpointID == "" {
		p, inferErr := svc.CurrentPath(ctx, r.Cwd, opt.One("path"))
		if inferErr != nil {
			return nil, "", inferErr
		}
		checkpointID = p.LatestCheckpointID
	}
	if checkpointID == "" {
		return nil, "", errors.New("the current path has no checkpoint")
	}
	switch args[0] {
	case "list":
		comments, err := svc.Store.ListComments(ctx, checkpointID)
		if err != nil {
			return nil, "", err
		}
		prompt, err := svc.FeedbackMarkdown(ctx, checkpointID)
		if opt.Bool("prompt") {
			return comments, prompt, err
		}
		return comments, commentsText(comments), err
	case "add":
		input := service.CommentInput{CheckpointID: checkpointID, FilePath: opt.One("file"), Side: opt.One("side"), StartLine: opt.Int("start"), EndLine: opt.Int("end"), Body: opt.One("body")}
		c, err := svc.AddComment(ctx, input)
		return c, "Feedback added.", err
	case "resolve", "reopen":
		if len(opt.Args) != 1 {
			return nil, "", fmt.Errorf("usage: ooh feedback %s <comment-id>", args[0])
		}
		resolved := args[0] == "resolve"
		err := svc.ResolveComment(ctx, opt.Args[0], resolved)
		return map[string]any{"commentId": opt.Args[0], "resolved": resolved}, "Feedback updated.", err
	default:
		return nil, "", fmt.Errorf("unknown feedback command %q", args[0])
	}
}

func (r *Runner) integrate(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("usage: ooh integrate <prepare|show|resolve|apply>")
	}
	opt, err := parseOptions(args[1:])
	if err != nil {
		return nil, "", err
	}
	switch args[0] {
	case "prepare":
		taskID := ""
		if len(opt.Args) > 0 {
			taskID = opt.Args[0]
		}
		if taskID == "" {
			p, inferErr := svc.CurrentPath(ctx, r.Cwd, opt.One("path"))
			if inferErr != nil {
				return nil, "", inferErr
			}
			taskID = p.TaskID
		}
		candidate, err := svc.PrepareIntegration(ctx, taskID, opt.One("checkpoint"), opt.One("message"))
		if err != nil && candidate.ID == "" {
			return nil, "", err
		}
		if candidate.State == model.CandidateNeedsResolution {
			return candidate, fmt.Sprintf("Integration needs resolution.\nWorkspace: %s\nCandidate ID: %s", candidate.WorktreePath, candidate.ID), nil
		}
		return candidate, fmt.Sprintf("Integration candidate ready. Review it with:\n  ooh integrate show %s\nThen apply with:\n  ooh integrate apply %s --confirm %s", candidate.ID, candidate.ID, candidate.CommitOID), err
	case "show":
		if len(opt.Args) != 1 {
			return nil, "", errors.New("usage: ooh integrate show <candidate-id>")
		}
		candidate, err := svc.Store.GetCandidate(ctx, opt.Args[0])
		if err != nil {
			return nil, "", err
		}
		var diff service.DiffResult
		if candidate.CommitOID != "" {
			diff, _ = svc.CandidateDiff(ctx, candidate.ID, "")
		}
		data := map[string]any{"candidate": candidate, "diff": diff}
		return data, fmt.Sprintf("Candidate %s\nState: %s\nBase: %s @ %s\nCommit: %s\nWorkspace: %s", candidate.ID, candidate.State, candidate.BaseRef, shortOID(candidate.BaseOID), shortOID(candidate.CommitOID), candidate.WorktreePath), nil
	case "resolve":
		if len(opt.Args) != 1 {
			return nil, "", errors.New("usage: ooh integrate resolve <candidate-id>")
		}
		candidate, err := svc.ResolveCandidate(ctx, opt.Args[0])
		return candidate, fmt.Sprintf("Resolution candidate ready. Commit: %s", candidate.CommitOID), err
	case "apply":
		if len(opt.Args) != 1 || opt.One("confirm") == "" {
			return nil, "", errors.New("usage: ooh integrate apply <candidate-id> --confirm <commit-oid>")
		}
		integration, err := svc.ApplyIntegration(ctx, opt.Args[0], opt.One("confirm"))
		return integration, fmt.Sprintf("Task integrated as one commit: %s\nUndo ID: %s", shortOID(integration.CommitOID), integration.ID), err
	default:
		return nil, "", fmt.Errorf("unknown integrate command %q", args[0])
	}
}

func (r *Runner) archive(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("usage: ooh archive <list|keep|release|cleanup>")
	}
	switch args[0] {
	case "list":
		paths, err := svc.ArchivedPaths(ctx)
		var b strings.Builder
		for _, p := range paths {
			fmt.Fprintf(&b, "%s  %s  keep=%t\n", shortID(p.ID), p.Name, p.Keep)
		}
		if len(paths) == 0 {
			b.WriteString("No archived paths.")
		}
		return paths, strings.TrimRight(b.String(), "\n"), err
	case "keep", "release":
		if len(args) != 2 {
			return nil, "", fmt.Errorf("usage: ooh archive %s <path-id>", args[0])
		}
		p, err := svc.SetPathKeep(ctx, args[1], args[0] == "keep")
		return p, "Archive retention updated.", err
	case "cleanup":
		ids, err := svc.CleanupExpired(ctx)
		return map[string]any{"deletedPathIds": ids}, fmt.Sprintf("Removed %d expired path(s).", len(ids)), err
	default:
		return nil, "", fmt.Errorf("unknown archive command %q", args[0])
	}
}

func (r *Runner) agent(svc *service.Service, args []string) (any, string, error) {
	if len(args) == 0 || args[0] != "setup" {
		return nil, "", errors.New("usage: ooh agent setup [--scope project|user] [--yes] [--force]")
	}
	opt, err := parseOptions(args[1:], "yes", "force")
	if err != nil {
		return nil, "", err
	}
	userScope := opt.One("scope") == "user"
	preview, err := agentskill.Preview(svc.Repo.Root, userScope)
	if err != nil {
		return nil, "", err
	}
	if !opt.Bool("yes") {
		return preview, fmt.Sprintf("Agent Skill preview\nDestination: %s\nFiles: %s\nRun again with --yes to install.", preview.Destination, strings.Join(preview.Files, ", ")), nil
	}
	result, err := agentskill.Install(svc.Repo.Root, userScope, opt.Bool("force"))
	return result, "Ooh Agent Skill installed at: " + result.Destination, err
}

func (r *Runner) doctor(ctx context.Context, svc *service.Service, args []string) (any, string, error) {
	opt, err := parseOptions(args, "bundle")
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{"repository": filepath.Base(svc.Repo.Root), "repoId": svc.RepoID, "stateDir": svc.StateDir, "workspaceRoot": svc.WorkspaceRoot, "gitVersion": gitVersion(ctx, svc)}
	if opt.Bool("bundle") {
		path, err := svc.DiagnosticBundle(ctx, opt.One("output"))
		if err != nil {
			return nil, "", err
		}
		result["bundle"] = path
		return result, "Diagnostic bundle created: " + path, nil
	}
	return result, fmt.Sprintf("Ooh is healthy.\nRepository: %s\nGit: %s\nState: %s", filepath.Base(svc.Repo.Root), result["gitVersion"], svc.StateDir), nil
}

func gitVersion(ctx context.Context, svc *service.Service) string {
	out, err := svc.Git.Output(ctx, svc.Repo.Root, "--version")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(out)
}

type options struct {
	Args   []string
	values map[string][]string
	bools  map[string]bool
}

func parseOptions(args []string, boolNames ...string) (options, error) {
	boolSet := map[string]bool{"no-open": true, "yes": true, "force": true, "bundle": true, "prompt": true}
	for _, name := range boolNames {
		boolSet[name] = true
	}
	opt := options{values: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opt.Args = append(opt.Args, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			opt.Args = append(opt.Args, arg)
			continue
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if boolSet[name] {
			if hasValue {
				opt.bools[name] = value != "false"
			} else {
				opt.bools[name] = true
			}
			continue
		}
		if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return opt, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		opt.values[name] = append(opt.values[name], value)
	}
	return opt, nil
}

func (o options) One(name string) string {
	values := o.values[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}
func (o options) Many(name string) []string { return append([]string(nil), o.values[name]...) }
func (o options) Bool(name string) bool     { return o.bools[name] }
func (o options) Int(name string) int       { value, _ := strconv.Atoi(o.One(name)); return value }

func removeBoolFlag(args *[]string, target string) bool {
	found := false
	result := (*args)[:0]
	for _, arg := range *args {
		if arg == target {
			found = true
			continue
		}
		result = append(result, arg)
	}
	*args = result
	return found
}

func (r *Runner) success(jsonMode bool, data any, human string) {
	if jsonMode {
		_ = json.NewEncoder(r.Stdout).Encode(map[string]any{"schemaVersion": "1", "data": data, "warnings": []string{}})
		return
	}
	if human != "" {
		fmt.Fprintln(r.Stdout, human)
		return
	}
	bytes, _ := json.MarshalIndent(data, "", "  ")
	fmt.Fprintln(r.Stdout, string(bytes))
}

func (r *Runner) fail(jsonMode bool, err error) int {
	code, exit, recovery := classifyError(err)
	if jsonMode {
		_ = json.NewEncoder(r.Stdout).Encode(map[string]any{"schemaVersion": "1", "error": map[string]any{"code": code, "message": err.Error(), "recovery": recovery}})
	} else {
		fmt.Fprintln(r.Stderr, "Ooh couldn't complete that:", err)
		if recovery != "" {
			fmt.Fprintln(r.Stderr, "Try:", recovery)
		}
	}
	return exit
}

func classifyError(err error) (string, int, string) {
	text := strings.ToLower(err.Error())
	var commandErr *gitexec.CommandError
	if errors.As(err, &commandErr) && (strings.Contains(text, "conflict") || strings.Contains(text, "merge")) {
		return "git_conflict", 4, "Open the reported resolution workspace or inspect the Git error."
	}
	if strings.Contains(text, "advanced") || strings.Contains(text, "confirmation") || strings.Contains(text, "still running") {
		return "state_conflict", 5, "Refresh task state and prepare the operation again."
	}
	if strings.Contains(text, "usage:") || strings.Contains(text, "unknown command") || strings.Contains(text, "requires a value") {
		return "usage_error", 2, "Run `ooh help` for command examples."
	}
	if strings.Contains(text, "required") || strings.Contains(text, "restore") || strings.Contains(text, "clean") || strings.Contains(text, "not inside") {
		return "precondition_failed", 3, "Address the stated repository condition and retry."
	}
	return "internal_error", 6, "Run `ooh doctor --bundle` and attach the reviewed bundle to a bug report if the problem persists."
}

func taskDetailText(detail model.TaskDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nState: %s\nTask ID: %s\nBase: %s @ %s\n", detail.Task.Title, detail.Task.State, detail.Task.ID, detail.Task.BaseRef, shortOID(detail.Task.BaseOID))
	if detail.Task.Description != "" {
		fmt.Fprintf(&b, "Goal: %s\n", detail.Task.Description)
	}
	b.WriteString("Paths:\n")
	for _, p := range detail.Paths {
		fmt.Fprintf(&b, "- %s [%s]\n  %s\n  %s\n", p.Name, p.State, p.BranchRef, fallback(p.WorktreePath, "archived"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func commentsText(comments []model.Comment) string {
	var b strings.Builder
	for _, c := range comments {
		location := "General"
		if c.FilePath != "" {
			location = fmt.Sprintf("%s:L%d-L%d", c.FilePath, c.StartLine, c.EndLine)
		}
		fmt.Fprintf(&b, "[%s] %s\n%s\n\n", c.Status, location, c.Body)
	}
	if len(comments) == 0 {
		return "No feedback on this checkpoint."
	}
	return strings.TrimSpace(b.String())
}

func shortID(value string) string {
	if len(value) > 20 {
		return value[:20]
	}
	return value
}
func shortOID(value string) string {
	if len(value) > 10 {
		return value[:10]
	}
	return value
}
func fallback(value, other string) string {
	if value == "" {
		return other
	}
	return value
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const usage = `Ooh turns parallel agent work into reviewable tasks and paths.

Usage:
  ooh                              Open the local dashboard
  ooh task create "Title"           Create an isolated task workspace
  ooh task import <branch>         Import an existing local branch
  ooh task list                    List tasks
  ooh task show [task-id]          Show task, paths, and checkpoints
  ooh path fork "Path B" [...]      Fork the current path at a checkpoint
  ooh path select <path-or-cp>     Choose a path or checkpoint
  ooh checkpoint create [name]     Capture all live, non-ignored work
  ooh ready --summary ...          Mark a frozen checkpoint ready
  ooh feedback list [--prompt]     Read checkpoint feedback
  ooh integrate prepare [task-id]  Create a safe squash candidate
  ooh integrate show <candidate>   Review the prepared candidate
  ooh integrate apply <candidate> --confirm <oid>
  ooh undo <integration-id>        Safely undo the latest integration
  ooh agent setup                  Preview the portable Agent Skill
  ooh doctor [--bundle]            Check local state or export diagnostics

Global options:
  --json                           Emit the stable JSON envelope
`
