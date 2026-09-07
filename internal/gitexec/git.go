package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/keyanfayaz/git-ooh/internal/model"
)

type CommandError struct {
	Args     []string
	Dir      string
	ExitCode int
	Output   string
}

func (e *CommandError) Error() string {
	message := strings.TrimSpace(e.Output)
	if message == "" {
		message = fmt.Sprintf("git exited with status %d", e.ExitCode)
	}
	return message
}

type Repo struct {
	Root      string `json:"root"`
	GitDir    string `json:"gitDir"`
	CommonDir string `json:"commonDir"`
}

type Worktree struct {
	Path      string
	HeadOID   string
	BranchRef string
	Detached  bool
	Locked    bool
}

type Client struct {
	Executable string
}

func New() *Client { return &Client{Executable: "git"} }

func Discover(ctx context.Context, dir string) (Repo, error) {
	c := New()
	root, err := c.Output(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return Repo{}, fmt.Errorf("not inside a Git working tree: %w", err)
	}
	bare, err := c.Output(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return Repo{}, err
	}
	if strings.TrimSpace(bare) == "true" {
		return Repo{}, errors.New("bare repositories are not supported")
	}
	gitDir, err := c.Output(ctx, dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return Repo{}, err
	}
	common, err := c.Output(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repo{}, err
	}
	return Repo{Root: strings.TrimSpace(root), GitDir: strings.TrimSpace(gitDir), CommonDir: strings.TrimSpace(common)}, nil
}

func (c *Client) Output(ctx context.Context, dir string, args ...string) (string, error) {
	stdout, stderr, err := c.run(ctx, dir, nil, args...)
	if err != nil {
		return "", commandError(dir, args, stdout, stderr, err)
	}
	return string(stdout), nil
}

func (c *Client) Run(ctx context.Context, dir string, args ...string) error {
	stdout, stderr, err := c.run(ctx, dir, nil, args...)
	if err != nil {
		return commandError(dir, args, stdout, stderr, err)
	}
	return nil
}

func (c *Client) run(ctx context.Context, dir string, env []string, args ...string) ([]byte, []byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, c.Executable, full...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func commandError(dir string, args []string, stdout, stderr []byte, err error) error {
	exit := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	}
	output := string(stderr)
	if strings.TrimSpace(output) == "" {
		output = string(stdout)
	}
	return &CommandError{Args: args, Dir: dir, ExitCode: exit, Output: output}
}

func (c *Client) Resolve(ctx context.Context, repo Repo, ref string) (string, error) {
	out, err := c.Output(ctx, repo.Root, "rev-parse", "--verify", ref+"^{commit}")
	return strings.TrimSpace(out), err
}

func (c *Client) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := c.Output(ctx, dir, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", errors.New("detached HEAD; pass --base with a local branch")
	}
	return branch, nil
}

func (c *Client) MergeBase(ctx context.Context, repo Repo, a, b string) (string, error) {
	out, err := c.Output(ctx, repo.Root, "merge-base", a, b)
	return strings.TrimSpace(out), err
}

func (c *Client) ValidateBranchName(ctx context.Context, repo Repo, name string) error {
	return c.Run(ctx, repo.Root, "check-ref-format", "--branch", name)
}

func (c *Client) AddWorktree(ctx context.Context, repo Repo, path, branchName, start string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return c.Run(ctx, repo.Root, "worktree", "add", "--lock", "--reason", "managed by ooh", "-b", branchName, path, start)
}

// CloneWorkspace creates a fully separate local clone for an Ooh path. --no-hardlinks
// prevents the clone from sharing object files with the source repository.
func (c *Client) CloneWorkspace(ctx context.Context, repo Repo, path, branchName, start string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Executable, "clone", "--no-hardlinks", "--no-checkout", "--quiet", repo.Root, path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return commandError(repo.Root, []string{"clone", "--no-hardlinks", "--no-checkout", repo.Root, path}, stdout.Bytes(), stderr.Bytes(), err)
	}
	branchName = strings.TrimPrefix(branchName, "refs/heads/")
	if err := c.Run(ctx, path, "checkout", "--quiet", "-b", branchName, start); err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	_ = c.Run(ctx, path, "remote", "remove", "origin")
	return nil
}

func (c *Client) ImportCommit(ctx context.Context, repo Repo, workspace, oid string) error {
	return c.Run(ctx, repo.Root, "fetch", "--quiet", "--no-tags", workspace, oid)
}

func (c *Client) AddExistingWorktree(ctx context.Context, repo Repo, path, branchRef string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return c.Run(ctx, repo.Root, "worktree", "add", "--lock", "--reason", "managed by ooh", path, branchRef)
}

func (c *Client) UnlockWorktree(ctx context.Context, repo Repo, path string) error {
	err := c.Run(ctx, repo.Root, "worktree", "unlock", path)
	if err != nil && strings.Contains(err.Error(), "not locked") {
		return nil
	}
	return err
}

func (c *Client) RemoveWorktree(ctx context.Context, repo Repo, path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		_ = c.Run(ctx, repo.Root, "worktree", "prune")
		return nil
	}
	if err := c.UnlockWorktree(ctx, repo, path); err != nil {
		return err
	}
	return c.Run(ctx, repo.Root, "worktree", "remove", path)
}

func (c *Client) DeleteBranch(ctx context.Context, repo Repo, branchRef string) error {
	branch := strings.TrimPrefix(branchRef, "refs/heads/")
	return c.Run(ctx, repo.Root, "branch", "-D", "--", branch)
}

func (c *Client) UpdateRef(ctx context.Context, repo Repo, ref, newOID string, oldOID ...string) error {
	args := []string{"update-ref", ref, newOID}
	args = append(args, oldOID...)
	return c.Run(ctx, repo.Root, args...)
}

func (c *Client) DeleteRef(ctx context.Context, repo Repo, ref string) error {
	return c.Run(ctx, repo.Root, "update-ref", "-d", ref)
}

func (c *Client) Worktrees(ctx context.Context, repo Repo) ([]Worktree, error) {
	out, err := c.Output(ctx, repo.Root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	var result []Worktree
	var current *Worktree
	flush := func() {
		if current != nil && current.Path != "" {
			result = append(result, *current)
		}
		current = nil
	}
	for _, part := range parts {
		part = strings.TrimSuffix(part, "\n")
		if part == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(part, " ")
		if key == "worktree" {
			flush()
			current = &Worktree{Path: value}
			continue
		}
		if current == nil {
			continue
		}
		switch key {
		case "HEAD":
			current.HeadOID = value
		case "branch":
			current.BranchRef = value
		case "detached":
			current.Detached = true
		case "locked":
			current.Locked = true
		}
	}
	flush()
	return result, nil
}

func (c *Client) WorktreeForBranch(ctx context.Context, repo Repo, branchRef string) (Worktree, bool, error) {
	all, err := c.Worktrees(ctx, repo)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, wt := range all {
		if wt.BranchRef == branchRef {
			return wt, true, nil
		}
	}
	return Worktree{}, false, nil
}

func (c *Client) Status(ctx context.Context, dir string) (model.WorktreeStatus, error) {
	branch, _ := c.Output(ctx, dir, "symbolic-ref", "-q", "HEAD")
	head, err := c.Output(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return model.WorktreeStatus{}, err
	}
	out, err := c.Output(ctx, dir, "status", "--porcelain=v2", "-z", "--untracked-files=normal")
	if err != nil {
		return model.WorktreeStatus{}, err
	}
	status := model.WorktreeStatus{Path: dir, BranchRef: strings.TrimSpace(branch), HeadOID: strings.TrimSpace(head)}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		status.Dirty = true
		status.Changed++
		if strings.HasPrefix(record, "? ") {
			status.Untracked++
		}
	}
	return status, nil
}

func (c *Client) HasConflicts(ctx context.Context, dir string) (bool, error) {
	out, err := c.Output(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func (c *Client) IndexDirty(ctx context.Context, dir string) (bool, error) {
	err := c.Run(ctx, dir, "diff", "--cached", "--quiet", "--exit-code")
	if err == nil {
		return false, nil
	}
	var ce *CommandError
	if errors.As(err, &ce) && ce.ExitCode == 1 {
		return true, nil
	}
	return false, err
}

func (c *Client) Checkpoint(ctx context.Context, repo Repo, dir, checkpointRef, message string) (string, bool, error) {
	conflicts, err := c.HasConflicts(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if conflicts {
		return "", false, errors.New("cannot create a checkpoint while merge conflicts are unresolved")
	}
	if err := c.Run(ctx, dir, "add", "-A"); err != nil {
		return "", false, err
	}
	dirty := true
	if err := c.Run(ctx, dir, "diff", "--cached", "--quiet", "--exit-code"); err == nil {
		dirty = false
	} else if ce := new(CommandError); !errors.As(err, &ce) || ce.ExitCode != 1 {
		return "", false, err
	}
	if dirty {
		args := []string{"-c", "commit.gpgSign=false"}
		name, _ := c.Output(ctx, dir, "config", "--get", "user.name")
		email, _ := c.Output(ctx, dir, "config", "--get", "user.email")
		if strings.TrimSpace(name) == "" {
			args = append(args, "-c", "user.name=Ooh")
		}
		if strings.TrimSpace(email) == "" {
			args = append(args, "-c", "user.email=ooh@local")
		}
		args = append(args, "commit", "--no-verify", "-m", message)
		if err := c.Run(ctx, dir, args...); err != nil {
			return "", false, err
		}
	}
	oid, err := c.Output(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", false, err
	}
	oid = strings.TrimSpace(oid)
	workspaceCommon, _ := c.Output(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if filepath.Clean(strings.TrimSpace(workspaceCommon)) != filepath.Clean(repo.CommonDir) {
		if err := c.ImportCommit(ctx, repo, dir, oid); err != nil {
			return "", false, fmt.Errorf("import isolated workspace commit: %w", err)
		}
	}
	if err := c.UpdateRef(ctx, repo, checkpointRef, oid); err != nil {
		return "", false, err
	}
	return oid, dirty, nil
}

func (c *Client) Diff(ctx context.Context, repo Repo, baseOID, targetOID, path string) (string, error) {
	args := []string{"diff", "--no-ext-diff", "--find-renames", "--unified=3", baseOID, targetOID}
	if path != "" {
		args = append(args, "--", path)
	}
	return c.Output(ctx, repo.Root, args...)
}

func (c *Client) ChangedFiles(ctx context.Context, repo Repo, baseOID, targetOID string) ([]model.ChangedFile, error) {
	statusOut, err := c.Output(ctx, repo.Root, "diff", "--no-ext-diff", "--find-renames", "--name-status", "-z", baseOID, targetOID)
	if err != nil {
		return nil, err
	}
	statsOut, err := c.Output(ctx, repo.Root, "diff", "--no-ext-diff", "--numstat", "-z", baseOID, targetOID)
	if err != nil {
		return nil, err
	}
	stats := parseNumstat(statsOut)
	parts := splitNonEmpty(statusOut)
	var files []model.ChangedFile
	for i := 0; i < len(parts); {
		status := parts[i]
		i++
		if i >= len(parts) {
			break
		}
		f := model.ChangedFile{Status: status[:1]}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			f.OldPath = parts[i]
			i++
			if i >= len(parts) {
				break
			}
		}
		f.Path = parts[i]
		i++
		if stat, ok := stats[f.Path]; ok {
			f.Additions, f.Deletions, f.Binary = stat.add, stat.del, stat.binary
		}
		files = append(files, f)
	}
	return files, nil
}

type fileStat struct {
	add, del int
	binary   bool
}

func parseNumstat(out string) map[string]fileStat {
	result := map[string]fileStat{}
	parts := splitNonEmpty(out)
	for _, entry := range parts {
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		stat := fileStat{}
		if fields[0] == "-" || fields[1] == "-" {
			stat.binary = true
		} else {
			stat.add, _ = strconv.Atoi(fields[0])
			stat.del, _ = strconv.Atoi(fields[1])
		}
		result[fields[2]] = stat
	}
	return result
}

func splitNonEmpty(out string) []string {
	raw := strings.Split(out, "\x00")
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func (c *Client) MergeSquash(ctx context.Context, dir, chosenOID string) (bool, error) {
	err := c.Run(ctx, dir, "merge", "--squash", "--no-commit", chosenOID)
	if err == nil {
		return false, nil
	}
	conflicts, statusErr := c.HasConflicts(ctx, dir)
	if statusErr != nil {
		return false, err
	}
	if conflicts {
		return true, nil
	}
	return false, err
}

func (c *Client) Commit(ctx context.Context, dir, message string, internal bool) (string, error) {
	args := []string{}
	if internal {
		args = append(args, "-c", "commit.gpgSign=false")
	}
	args = append(args, "commit")
	if internal {
		args = append(args, "--no-verify")
	}
	args = append(args, "-m", message)
	stdout, stderr, err := c.run(ctx, dir, []string{"GIT_TERMINAL_PROMPT=0"}, args...)
	if err != nil {
		return "", commandError(dir, args, stdout, stderr, err)
	}
	out, err := c.Output(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

func (c *Client) FastForward(ctx context.Context, repo Repo, baseRef, expectedOID, commitOID string) error {
	if err := c.ValidateFastForward(ctx, repo, baseRef, expectedOID); err != nil {
		return err
	}
	wt, ok, err := c.WorktreeForBranch(ctx, repo, baseRef)
	if err != nil {
		return err
	}
	if !ok {
		return c.UpdateRef(ctx, repo, baseRef, commitOID, expectedOID)
	}
	return c.Run(ctx, wt.Path, "merge", "--ff-only", commitOID)
}

func (c *Client) ValidateFastForward(ctx context.Context, repo Repo, baseRef, expectedOID string) error {
	current, err := c.Resolve(ctx, repo, baseRef)
	if err != nil {
		return err
	}
	if current != expectedOID {
		return errors.New("the base branch advanced; prepare a new integration candidate")
	}
	wt, ok, err := c.WorktreeForBranch(ctx, repo, baseRef)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	status, err := c.Status(ctx, wt.Path)
	if err != nil {
		return err
	}
	if status.Dirty {
		return errors.New("the base worktree has uncommitted changes; clean it before integration")
	}
	if status.HeadOID != expectedOID {
		return errors.New("the base branch advanced; prepare a new integration candidate")
	}
	return nil
}

func (c *Client) ResetHard(ctx context.Context, repo Repo, baseRef, expectedOID, targetOID string) error {
	wt, ok, err := c.WorktreeForBranch(ctx, repo, baseRef)
	if err != nil {
		return err
	}
	if !ok {
		return c.UpdateRef(ctx, repo, baseRef, targetOID, expectedOID)
	}
	status, err := c.Status(ctx, wt.Path)
	if err != nil {
		return err
	}
	if status.Dirty || status.HeadOID != expectedOID {
		return errors.New("safe undo requires the integration commit to be the clean base tip")
	}
	return c.Run(ctx, wt.Path, "reset", "--hard", targetOID)
}
