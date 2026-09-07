package service

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/keyanfayaz/git-ooh/internal/gitexec"
	"github.com/keyanfayaz/git-ooh/internal/id"
	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/operationlock"
	"github.com/keyanfayaz/git-ooh/internal/store"
)

const (
	maxArtifacts     = 10
	maxArtifactBytes = 25 << 20
)

type Service struct {
	Repo          gitexec.Repo
	Git           *gitexec.Client
	Store         *store.Store
	StateDir      string
	WorkspaceRoot string
	RepoID        string
	Bus           *Bus
	Runs          *RunManager
	Previews      *PreviewManager
}

type CheckpointInput struct {
	Name               string   `json:"name"`
	Summary            string   `json:"summary"`
	VerificationStatus string   `json:"verificationStatus"`
	VerificationNotes  string   `json:"verificationNotes"`
	RiskNotes          string   `json:"riskNotes"`
	Ready              bool     `json:"ready"`
	ArtifactPaths      []string `json:"artifactPaths,omitempty"`
}

type CommentInput struct {
	CheckpointID string `json:"checkpointId"`
	FilePath     string `json:"filePath"`
	Side         string `json:"side"`
	StartLine    int    `json:"startLine"`
	EndLine      int    `json:"endLine"`
	Body         string `json:"body"`
}

type DiffResult struct {
	BaselineOID string              `json:"baselineOid"`
	TargetOID   string              `json:"targetOid"`
	Files       []model.ChangedFile `json:"files"`
	Patch       string              `json:"patch"`
}

type workspaceLink struct {
	RepoRoot string `json:"repoRoot"`
	PathID   string `json:"pathId"`
}

func Open(ctx context.Context, dir string) (*Service, error) {
	repo, err := gitexec.Discover(ctx, dir)
	if err != nil {
		return nil, err
	}
	git := gitexec.New()
	if data, readErr := os.ReadFile(filepath.Join(repo.GitDir, "ooh-parent.json")); readErr == nil {
		var link workspaceLink
		if json.Unmarshal(data, &link) == nil && link.RepoRoot != "" {
			if parent, discoverErr := gitexec.Discover(ctx, link.RepoRoot); discoverErr == nil {
				repo = parent
			}
		}
	}
	if _, err := git.Resolve(ctx, repo, "HEAD"); err != nil {
		return nil, errors.New("Ooh requires a repository with at least one commit")
	}
	stateDir := filepath.Join(repo.CommonDir, "ooh")
	st, err := store.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return nil, err
	}
	repoID, err := st.Setting(ctx, "repo_id")
	if errors.Is(err, store.ErrNotFound) {
		repoID = id.New("repo")
		if err := st.SetSetting(ctx, "repo_id", repoID); err != nil {
			st.Close()
			return nil, err
		}
		_ = st.SetSetting(ctx, "retention_days", "30")
	} else if err != nil {
		st.Close()
		return nil, err
	}
	workspaceRoot, err := workspaceRoot(repoID)
	if err != nil {
		st.Close()
		return nil, err
	}
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		st.Close()
		return nil, err
	}
	_ = st.InterruptActiveRuns(ctx)
	svc := &Service{Repo: repo, Git: git, Store: st, StateDir: stateDir, WorkspaceRoot: workspaceRoot, RepoID: repoID, Bus: NewBus()}
	svc.Runs = NewRunManager(svc)
	svc.Previews = NewPreviewManager(svc)
	_, _ = svc.CleanupExpired(ctx)
	_, _ = svc.CleanupDrafts(ctx, time.Now().UTC().Add(-24*time.Hour))
	return svc, nil
}

func (s *Service) Close() error {
	if s.Runs != nil {
		s.Runs.Close()
	}
	if s.Previews != nil {
		s.Previews.Close()
	}
	return s.Store.Close()
}

func workspaceRoot(repoID string) (string, error) {
	var root string
	if override := os.Getenv("OOH_DATA_HOME"); override != "" {
		root = override
	} else if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, "Library", "Application Support", "ooh", "workspaces")
	} else if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		root = filepath.Join(data, "ooh", "workspaces")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "share", "ooh", "workspaces")
	}
	return filepath.Join(root, repoID), nil
}

func (s *Service) withLock(fn func() error) error {
	release, err := operationlock.Acquire(filepath.Join(s.StateDir, "operation.lock"), 10*time.Second)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

func (s *Service) createPathWorkspace(ctx context.Context, path, branchRef, startOID, pathID string) error {
	if err := s.Git.UpdateRef(ctx, s.Repo, branchRef, startOID); err != nil {
		return err
	}
	if err := s.Git.CloneWorkspace(ctx, s.Repo, path, branchRef, startOID); err != nil {
		_ = s.Git.DeleteBranch(ctx, s.Repo, branchRef)
		return err
	}
	data, _ := json.Marshal(workspaceLink{RepoRoot: s.Repo.Root, PathID: pathID})
	if err := os.WriteFile(filepath.Join(path, ".git", "ooh-parent.json"), data, 0o600); err != nil {
		_ = os.RemoveAll(path)
		_ = s.Git.DeleteBranch(ctx, s.Repo, branchRef)
		return err
	}
	return nil
}

func (s *Service) removePathWorkspace(path string) error {
	if path == "" {
		return nil
	}
	rel, err := filepath.Rel(s.WorkspaceRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("refusing to remove a workspace outside Ooh's data directory")
	}
	repo, discoverErr := gitexec.Discover(context.Background(), path)
	if discoverErr == nil && filepath.Clean(repo.CommonDir) == filepath.Clean(s.Repo.CommonDir) {
		return s.Git.RemoveWorktree(context.Background(), s.Repo, path)
	}
	return os.RemoveAll(path)
}

func (s *Service) inferBase(ctx context.Context, dir, base string) (string, error) {
	if base != "" {
		if strings.HasPrefix(base, "refs/heads/") {
			base = strings.TrimPrefix(base, "refs/heads/")
		}
		if _, err := s.Git.Resolve(ctx, s.Repo, "refs/heads/"+base); err != nil {
			return "", fmt.Errorf("base must be an existing local branch: %w", err)
		}
		return base, nil
	}
	currentRepo, err := gitexec.Discover(ctx, dir)
	if err == nil {
		if p, pathErr := s.Store.PathByWorktree(ctx, currentRepo.Root); pathErr == nil {
			task, taskErr := s.Store.GetTask(ctx, p.TaskID)
			if taskErr == nil {
				return strings.TrimPrefix(task.BaseRef, "refs/heads/"), nil
			}
		}
	}
	return s.Git.CurrentBranch(ctx, dir)
}

func (s *Service) CreateTask(ctx context.Context, dir, title, description, base string) (model.TaskDetail, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return model.TaskDetail{}, errors.New("task title is required")
	}
	base, err := s.inferBase(ctx, dir, base)
	if err != nil {
		return model.TaskDetail{}, err
	}
	baseRef := "refs/heads/" + base
	baseOID, err := s.Git.Resolve(ctx, s.Repo, baseRef)
	if err != nil {
		return model.TaskDetail{}, err
	}
	now := time.Now().UTC()
	taskID, pathID := id.New("task"), id.New("path")
	branch := fmt.Sprintf("ooh/%s/%s-%s", id.Short(taskID), id.Short(pathID), id.Slug(title))
	worktree := filepath.Join(s.WorkspaceRoot, id.Short(taskID), id.Short(pathID)+"-"+id.Slug(title))
	task := model.Task{ID: taskID, Title: title, Description: strings.TrimSpace(description), BaseRef: baseRef, BaseOID: baseOID, BaseTipOID: baseOID, State: model.TaskWorking, SelectedPathID: pathID, CreatedAt: now, UpdatedAt: now}
	path := model.Path{ID: pathID, TaskID: taskID, Name: "Primary", BranchRef: "refs/heads/" + branch, WorktreePath: worktree, State: model.PathWorking, CreatedAt: now, UpdatedAt: now}
	err = s.withLock(func() error {
		if err := s.createPathWorkspace(ctx, worktree, path.BranchRef, baseOID, pathID); err != nil {
			return err
		}
		if err := s.Store.CreateTaskWithPath(ctx, task, path); err != nil {
			_ = s.removePathWorkspace(worktree)
			_ = s.Git.DeleteBranch(ctx, s.Repo, path.BranchRef)
			return err
		}
		return s.Store.Audit(ctx, "task.created", taskID, "")
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("task.created", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) ImportTask(ctx context.Context, dir, source, title, description, base string) (model.TaskDetail, error) {
	if source == "" {
		return model.TaskDetail{}, errors.New("source branch is required")
	}
	base, err := s.inferBase(ctx, dir, base)
	if err != nil {
		return model.TaskDetail{}, err
	}
	baseRef := "refs/heads/" + base
	baseTip, err := s.Git.Resolve(ctx, s.Repo, baseRef)
	if err != nil {
		return model.TaskDetail{}, err
	}
	sourceOID, err := s.Git.Resolve(ctx, s.Repo, source)
	if err != nil {
		return model.TaskDetail{}, fmt.Errorf("resolve imported branch: %w", err)
	}
	diffBase, err := s.Git.MergeBase(ctx, s.Repo, baseTip, sourceOID)
	if err != nil {
		return model.TaskDetail{}, errors.New("the imported branch does not share history with the selected base")
	}
	if strings.TrimSpace(title) == "" {
		title = "Imported " + strings.TrimPrefix(source, "refs/heads/")
	}
	now := time.Now().UTC()
	taskID, pathID, cpID := id.New("task"), id.New("path"), id.New("cp")
	branch := fmt.Sprintf("ooh/%s/%s-%s", id.Short(taskID), id.Short(pathID), id.Slug(title))
	worktree := filepath.Join(s.WorkspaceRoot, id.Short(taskID), id.Short(pathID)+"-"+id.Slug(title))
	task := model.Task{ID: taskID, Title: title, Description: description, BaseRef: baseRef, BaseOID: diffBase, BaseTipOID: baseTip, State: model.TaskReady, SelectedPathID: pathID, SelectedCheckpointID: cpID, CreatedAt: now, UpdatedAt: now}
	path := model.Path{ID: pathID, TaskID: taskID, Name: "Imported path", BranchRef: "refs/heads/" + branch, WorktreePath: worktree, State: model.PathWorking, CreatedAt: now, UpdatedAt: now}
	cp := model.Checkpoint{ID: cpID, TaskID: taskID, PathID: pathID, Name: "Imported state", CommitOID: sourceOID, Summary: "Imported from " + source, VerificationStatus: "not-run", VerificationNotes: "No verification was supplied during import.", Ready: true, CreatedAt: now}
	err = s.withLock(func() error {
		if err := s.createPathWorkspace(ctx, worktree, path.BranchRef, sourceOID, pathID); err != nil {
			return err
		}
		if err := s.Store.CreateTaskWithPath(ctx, task, path); err != nil {
			_ = s.removePathWorkspace(worktree)
			_ = s.Git.DeleteBranch(ctx, s.Repo, path.BranchRef)
			return err
		}
		if err := s.Git.UpdateRef(ctx, s.Repo, "refs/ooh/checkpoints/"+cpID, sourceOID); err != nil {
			return err
		}
		return s.Store.CreateCheckpoint(ctx, cp)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("task.imported", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) CurrentPath(ctx context.Context, dir, pathID string) (model.Path, error) {
	if pathID != "" {
		return s.Store.GetPath(ctx, pathID)
	}
	repo, err := gitexec.Discover(ctx, dir)
	if err != nil {
		return model.Path{}, err
	}
	p, err := s.Store.PathByWorktree(ctx, repo.Root)
	if errors.Is(err, store.ErrNotFound) {
		currentRoot, _ := filepath.EvalSymlinks(repo.Root)
		tasks, _ := s.Store.ListTasks(ctx)
		for _, task := range tasks {
			paths, _ := s.Store.ListPaths(ctx, task.ID)
			for _, candidate := range paths {
				candidateRoot, _ := filepath.EvalSymlinks(candidate.WorktreePath)
				if currentRoot != "" && currentRoot == candidateRoot {
					return candidate, nil
				}
			}
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return model.Path{}, errors.New("run this command in an Ooh workspace or pass --path")
	}
	return p, err
}

func (s *Service) CreateCheckpoint(ctx context.Context, p model.Path, input CheckpointInput) (model.Checkpoint, error) {
	var cp model.Checkpoint
	err := s.withLock(func() error {
		var err error
		cp, err = s.checkpointUnlocked(ctx, p, input)
		return err
	})
	if err != nil {
		return model.Checkpoint{}, err
	}
	for _, artifact := range input.ArtifactPaths {
		if _, err := s.AddArtifactPath(ctx, cp.ID, p, artifact); err != nil {
			return cp, fmt.Errorf("checkpoint created, but artifact could not be attached: %w", err)
		}
	}
	s.Bus.Publish("checkpoint.created", cp.ID)
	return cp, nil
}

func (s *Service) checkpointUnlocked(ctx context.Context, p model.Path, input CheckpointInput) (model.Checkpoint, error) {
	if p.State == model.PathArchived || p.State == model.PathDeleted || p.WorktreePath == "" {
		return model.Checkpoint{}, errors.New("restore the path before creating a checkpoint")
	}
	if input.Ready {
		if strings.TrimSpace(input.Summary) == "" {
			return model.Checkpoint{}, errors.New("a ready checkpoint requires a plain-language summary")
		}
		if !validVerification(input.VerificationStatus) || strings.TrimSpace(input.VerificationNotes) == "" {
			return model.Checkpoint{}, errors.New("a ready checkpoint requires passed, failed, or not-run verification with notes")
		}
	}
	if input.VerificationStatus == "" {
		input.VerificationStatus = "not-run"
	}
	if input.Name == "" {
		if input.Ready {
			input.Name = "Ready " + time.Now().Format("2006-01-02 15:04")
		} else {
			input.Name = "Checkpoint " + time.Now().Format("2006-01-02 15:04")
		}
	}
	cpID := id.New("cp")
	oid, _, err := s.Git.Checkpoint(ctx, s.Repo, p.WorktreePath, "refs/ooh/checkpoints/"+cpID, "ooh checkpoint: "+input.Name)
	if err != nil {
		return model.Checkpoint{}, err
	}
	now := time.Now().UTC()
	cp := model.Checkpoint{ID: cpID, TaskID: p.TaskID, PathID: p.ID, ParentCheckpointID: p.LatestCheckpointID, Name: input.Name, CommitOID: oid, Summary: strings.TrimSpace(input.Summary), VerificationStatus: input.VerificationStatus, VerificationNotes: strings.TrimSpace(input.VerificationNotes), RiskNotes: strings.TrimSpace(input.RiskNotes), Ready: input.Ready, CreatedAt: now}
	if err := s.Git.UpdateRef(ctx, s.Repo, p.BranchRef, oid); err != nil {
		_ = s.Git.DeleteRef(ctx, s.Repo, "refs/ooh/checkpoints/"+cpID)
		return model.Checkpoint{}, err
	}
	if err := s.Store.CreateCheckpoint(ctx, cp); err != nil {
		_ = s.Git.DeleteRef(ctx, s.Repo, "refs/ooh/checkpoints/"+cpID)
		return model.Checkpoint{}, err
	}
	_ = s.Store.Audit(ctx, "checkpoint.created", cp.ID, "")
	return cp, nil
}

func validVerification(value string) bool {
	return value == "passed" || value == "failed" || value == "not-run"
}

func (s *Service) ForkPath(ctx context.Context, p model.Path, names []string, summary string) (model.TaskDetail, error) {
	if len(names) == 0 {
		return model.TaskDetail{}, errors.New("provide at least one new path name")
	}
	var result model.TaskDetail
	err := s.withLock(func() error {
		cp, err := s.checkpointUnlocked(ctx, p, CheckpointInput{Name: "Fork point", Summary: summary, VerificationStatus: "not-run", VerificationNotes: "Forked for parallel exploration."})
		if err != nil {
			return err
		}
		forkID := id.New("fork")
		pathIDs := []string{p.ID}
		created := []model.Path{}
		for _, rawName := range names {
			name := strings.TrimSpace(rawName)
			if name == "" {
				return errors.New("path names cannot be empty")
			}
			pathID := id.New("path")
			branch := fmt.Sprintf("ooh/%s/%s-%s", id.Short(p.TaskID), id.Short(pathID), id.Slug(name))
			worktree := filepath.Join(s.WorkspaceRoot, id.Short(p.TaskID), id.Short(pathID)+"-"+id.Slug(name))
			now := time.Now().UTC()
			next := model.Path{ID: pathID, TaskID: p.TaskID, Name: name, BranchRef: "refs/heads/" + branch, WorktreePath: worktree, State: model.PathWorking, LatestCheckpointID: cp.ID, CreatedAt: now, UpdatedAt: now}
			if err := s.createPathWorkspace(ctx, worktree, next.BranchRef, cp.CommitOID, pathID); err != nil {
				for _, made := range created {
					_ = s.removePathWorkspace(made.WorktreePath)
					_ = s.Git.DeleteBranch(ctx, s.Repo, made.BranchRef)
				}
				return err
			}
			if err := s.Store.CreatePath(ctx, next); err != nil {
				_ = s.removePathWorkspace(worktree)
				_ = s.Git.DeleteBranch(ctx, s.Repo, next.BranchRef)
				return err
			}
			created = append(created, next)
			pathIDs = append(pathIDs, pathID)
		}
		f := model.Fork{ID: forkID, TaskID: p.TaskID, CheckpointID: cp.ID, CreatedAt: time.Now().UTC()}
		if err := s.Store.CreateFork(ctx, f, pathIDs); err != nil {
			return err
		}
		_ = s.Store.Audit(ctx, "path.forked", p.ID, forkID)
		return nil
	})
	if err != nil {
		return result, err
	}
	s.Bus.Publish("path.forked", p.TaskID)
	return s.TaskDetail(ctx, p.TaskID)
}

func (s *Service) SelectCheckpoint(ctx context.Context, checkpointID string) (model.TaskDetail, error) {
	cp, err := s.Store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	selectedPath, err := s.Store.GetPath(ctx, cp.PathID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	err = s.withLock(func() error {
		f, siblings, siblingErr := s.Store.SiblingPaths(ctx, selectedPath.ID)
		if siblingErr == nil {
			for _, sibling := range siblings {
				if sibling.ID == selectedPath.ID || sibling.State == model.PathArchived || sibling.State == model.PathDeleted {
					continue
				}
				if err := s.archivePathUnlocked(ctx, sibling); err != nil {
					return err
				}
			}
			if err := s.Store.SelectForkPath(ctx, f.ID, selectedPath.ID); err != nil {
				return err
			}
		} else if !errors.Is(siblingErr, store.ErrNotFound) {
			return siblingErr
		}
		task, err := s.Store.GetTask(ctx, cp.TaskID)
		if err != nil {
			return err
		}
		task.SelectedPathID, task.SelectedCheckpointID, task.State = selectedPath.ID, cp.ID, model.TaskWorking
		if cp.Ready {
			task.State = model.TaskReady
		}
		if err := s.Store.UpdateTask(ctx, task); err != nil {
			return err
		}
		selectedPath.State = model.PathSelected
		if cp.Ready {
			selectedPath.State = model.PathReady
		}
		return s.Store.UpdatePath(ctx, selectedPath)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("checkpoint.selected", checkpointID)
	return s.TaskDetail(ctx, cp.TaskID)
}

func (s *Service) SelectResult(ctx context.Context, targetID string) (model.TaskDetail, error) {
	if cp, err := s.Store.GetCheckpoint(ctx, targetID); err == nil {
		return s.SelectCheckpoint(ctx, cp.ID)
	}
	p, err := s.Store.GetPath(ctx, targetID)
	if err != nil {
		return model.TaskDetail{}, errors.New("selection target must be a path or checkpoint ID")
	}
	if p.LatestCheckpointID == "" {
		if p.State == model.PathArchived {
			if _, err := s.RestorePath(ctx, p.ID); err != nil {
				return model.TaskDetail{}, err
			}
			p, _ = s.Store.GetPath(ctx, p.ID)
		}
		cp, err := s.CreateCheckpoint(ctx, p, CheckpointInput{Name: "Selected state", VerificationStatus: "not-run", VerificationNotes: "Captured automatically for selection."})
		if err != nil {
			return model.TaskDetail{}, err
		}
		p.LatestCheckpointID = cp.ID
	}
	return s.SelectCheckpoint(ctx, p.LatestCheckpointID)
}

func (s *Service) archivePathUnlocked(ctx context.Context, p model.Path) error {
	if p.State == model.PathArchived || p.State == model.PathDeleted {
		return nil
	}
	if p.WorktreePath != "" {
		status, err := s.Git.Status(ctx, p.WorktreePath)
		if err == nil && status.Dirty {
			updated, getErr := s.Store.GetPath(ctx, p.ID)
			if getErr != nil {
				return getErr
			}
			if _, err := s.checkpointUnlocked(ctx, updated, CheckpointInput{Name: "Archive snapshot", VerificationStatus: "not-run", VerificationNotes: "Captured automatically before archiving."}); err != nil {
				return err
			}
		}
		if err := s.removePathWorkspace(p.WorktreePath); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	p.WorktreePath, p.State, p.ArchivedAt = "", model.PathArchived, &now
	return s.Store.UpdatePath(ctx, p)
}

func (s *Service) ArchivePath(ctx context.Context, pathID string) error {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return err
	}
	err = s.withLock(func() error { return s.archivePathUnlocked(ctx, p) })
	if err == nil {
		s.Bus.Publish("path.archived", pathID)
	}
	return err
}

func (s *Service) RestorePath(ctx context.Context, pathID string) (model.Path, error) {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return model.Path{}, err
	}
	if p.State != model.PathArchived {
		return p, nil
	}
	worktree := filepath.Join(s.WorkspaceRoot, id.Short(p.TaskID), id.Short(p.ID)+"-"+id.Slug(p.Name))
	err = s.withLock(func() error {
		startOID, resolveErr := s.Git.Resolve(ctx, s.Repo, p.BranchRef)
		if resolveErr != nil {
			return resolveErr
		}
		if err := s.createPathWorkspace(ctx, worktree, p.BranchRef, startOID, p.ID); err != nil {
			return err
		}
		p.WorktreePath, p.State, p.ArchivedAt = worktree, model.PathWorking, nil
		return s.Store.UpdatePath(ctx, p)
	})
	if err == nil {
		s.Bus.Publish("path.restored", pathID)
	}
	return p, err
}

func (s *Service) ArchiveTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	err = s.withLock(func() error {
		paths, err := s.Store.ListPaths(ctx, taskID)
		if err != nil {
			return err
		}
		for _, p := range paths {
			if p.State != model.PathArchived && p.State != model.PathDeleted {
				if err := s.archivePathUnlocked(ctx, p); err != nil {
					return err
				}
			}
		}
		task.State = model.TaskArchived
		return s.Store.UpdateTask(ctx, task)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("task.archived", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) RestoreTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if task.SelectedPathID == "" {
		return model.TaskDetail{}, errors.New("task has no selected path to restore")
	}
	if _, err := s.RestorePath(ctx, task.SelectedPathID); err != nil {
		return model.TaskDetail{}, err
	}
	task.State = model.TaskWorking
	if err := s.Store.UpdateTask(ctx, task); err != nil {
		return model.TaskDetail{}, err
	}
	s.Bus.Publish("task.restored", taskID)
	return s.TaskDetail(ctx, taskID)
}

func (s *Service) ListTasks(ctx context.Context) ([]model.Task, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	for index := range tasks {
		runs, runErr := s.Store.ListAgentRuns(ctx, tasks[index].ID)
		if runErr != nil {
			return nil, runErr
		}
		for _, run := range runs {
			if run.State == model.RunQueued || run.State == model.RunRunning {
				tasks[index].ActiveRuns++
			}
		}
	}
	return tasks, nil
}

func (s *Service) TaskDetail(ctx context.Context, taskID string) (model.TaskDetail, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	paths, err := s.Store.ListPaths(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	forks, err := s.Store.ListForks(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	checkpoints, err := s.Store.ListCheckpoints(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	comments, err := s.Store.ListTaskComments(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	artifacts, err := s.Store.ListTaskArtifacts(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	runs, err := s.Store.ListAgentRuns(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	statuses := map[string]model.WorktreeStatus{}
	for _, p := range paths {
		if p.WorktreePath == "" || p.State == model.PathArchived || p.State == model.PathDeleted {
			continue
		}
		if status, statusErr := s.Git.Status(ctx, p.WorktreePath); statusErr == nil {
			statuses[p.ID] = status
		}
	}
	return model.TaskDetail{Task: task, Paths: paths, Forks: forks, Checkpoints: checkpoints, Comments: comments, Artifacts: artifacts, Statuses: statuses, Runs: runs}, nil
}

func (s *Service) CheckpointDiff(ctx context.Context, checkpointID, baselineMode, file string) (DiffResult, error) {
	cp, err := s.Store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return DiffResult{}, err
	}
	task, err := s.Store.GetTask(ctx, cp.TaskID)
	if err != nil {
		return DiffResult{}, err
	}
	baseline := task.BaseOID
	if baselineMode != "task" {
		forks, _ := s.Store.ListForks(ctx, task.ID)
		checkpoints, _ := s.Store.ListCheckpoints(ctx, task.ID)
		cpTimes := map[string]time.Time{}
		for _, item := range checkpoints {
			cpTimes[item.ID] = item.CreatedAt
		}
		var latestForkAt time.Time
		for _, f := range forks {
			at := cpTimes[f.CheckpointID]
			if at.IsZero() || at.After(cp.CreatedAt) || !at.After(latestForkAt) {
				continue
			}
			pathIDs, listErr := s.Store.ListForkPaths(ctx, f.ID)
			if listErr != nil {
				continue
			}
			for _, pathID := range pathIDs {
				if pathID == cp.PathID {
					if forkCP, getErr := s.Store.GetCheckpoint(ctx, f.CheckpointID); getErr == nil {
						baseline, latestForkAt = forkCP.CommitOID, at
					}
					break
				}
			}
		}
	}
	files, err := s.Git.ChangedFiles(ctx, s.Repo, baseline, cp.CommitOID)
	if err != nil {
		return DiffResult{}, err
	}
	patch, err := s.Git.Diff(ctx, s.Repo, baseline, cp.CommitOID, file)
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{BaselineOID: baseline, TargetOID: cp.CommitOID, Files: files, Patch: patch}, nil
}

func (s *Service) AddComment(ctx context.Context, input CommentInput) (model.Comment, error) {
	if strings.TrimSpace(input.Body) == "" {
		return model.Comment{}, errors.New("comment body is required")
	}
	cp, err := s.Store.GetCheckpoint(ctx, input.CheckpointID)
	if err != nil {
		return model.Comment{}, err
	}
	if input.FilePath != "" {
		if input.Side != "old" && input.Side != "new" {
			return model.Comment{}, errors.New("line comments require side old or new")
		}
		if input.StartLine < 1 {
			return model.Comment{}, errors.New("line comments require a positive start line")
		}
		if input.EndLine == 0 {
			input.EndLine = input.StartLine
		}
		if input.EndLine < input.StartLine {
			return model.Comment{}, errors.New("end line cannot precede start line")
		}
	}
	c := model.Comment{ID: id.New("comment"), CheckpointID: cp.ID, FilePath: input.FilePath, Side: input.Side, StartLine: input.StartLine, EndLine: input.EndLine, Body: strings.TrimSpace(input.Body), Status: model.CommentOpen, CreatedAt: time.Now().UTC()}
	if err := s.Store.CreateComment(ctx, c); err != nil {
		return model.Comment{}, err
	}
	p, _ := s.Store.GetPath(ctx, cp.PathID)
	p.State = model.PathChangesRequested
	_ = s.Store.UpdatePath(ctx, p)
	task, _ := s.Store.GetTask(ctx, cp.TaskID)
	task.State = model.TaskChangesRequested
	_ = s.Store.UpdateTask(ctx, task)
	s.Bus.Publish("comment.created", c.ID)
	return c, nil
}

func (s *Service) ResolveComment(ctx context.Context, commentID string, resolved bool) error {
	if err := s.Store.ResolveComment(ctx, commentID, resolved); err != nil {
		return err
	}
	s.Bus.Publish("comment.updated", commentID)
	return nil
}

func (s *Service) FeedbackMarkdown(ctx context.Context, checkpointID string) (string, error) {
	comments, err := s.Store.ListComments(ctx, checkpointID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range comments {
		if c.Status != model.CommentOpen {
			continue
		}
		if c.FilePath != "" {
			fmt.Fprintf(&b, "%s:L%d", c.FilePath, c.StartLine)
			if c.EndLine > c.StartLine {
				fmt.Fprintf(&b, "-L%d", c.EndLine)
			}
			b.WriteString("\n")
		}
		b.WriteString(c.Body)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Service) AddArtifactPath(ctx context.Context, checkpointID string, p model.Path, source string) (model.Artifact, error) {
	if !filepath.IsAbs(source) {
		source = filepath.Join(p.WorktreePath, source)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return model.Artifact{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return model.Artifact{}, errors.New("artifacts must be regular, non-symlink files")
	}
	realSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return model.Artifact{}, err
	}
	realRoot, err := filepath.EvalSymlinks(p.WorktreePath)
	if err != nil {
		return model.Artifact{}, err
	}
	rel, err := filepath.Rel(realRoot, realSource)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return model.Artifact{}, errors.New("artifact must be inside the managed workspace")
	}
	f, err := os.Open(realSource)
	if err != nil {
		return model.Artifact{}, err
	}
	defer f.Close()
	return s.AddArtifact(ctx, checkpointID, filepath.Base(realSource), f, info.Size())
}

func (s *Service) AddArtifact(ctx context.Context, checkpointID, name string, r io.Reader, size int64) (model.Artifact, error) {
	if _, err := s.Store.GetCheckpoint(ctx, checkpointID); err != nil {
		return model.Artifact{}, err
	}
	existing, err := s.Store.ListCheckpointArtifacts(ctx, checkpointID)
	if err != nil {
		return model.Artifact{}, err
	}
	var total int64
	for _, item := range existing {
		total += item.Size
	}
	if len(existing) >= maxArtifacts || size < 0 || total+size > maxArtifactBytes {
		return model.Artifact{}, errors.New("checkpoint artifacts are limited to 10 files and 25 MB total")
	}
	ext := strings.ToLower(filepath.Ext(name))
	allowed := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".md": true, ".txt": true, ".json": true, ".log": true}
	if !allowed[ext] {
		return model.Artifact{}, errors.New("unsupported artifact type; use PNG, JPEG, WebP, Markdown, text, JSON, or log files")
	}
	artifactID := id.New("artifact")
	cleanName := strings.ReplaceAll(filepath.Base(name), string(filepath.Separator), "-")
	dir := filepath.Join(s.StateDir, "artifacts", checkpointID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.Artifact{}, err
	}
	dest := filepath.Join(dir, artifactID+"-"+cleanName)
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return model.Artifact{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(r, maxArtifactBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written != size || total+written > maxArtifactBytes {
		_ = os.Remove(dest)
		if copyErr != nil {
			return model.Artifact{}, copyErr
		}
		if closeErr != nil {
			return model.Artifact{}, closeErr
		}
		return model.Artifact{}, errors.New("artifact size changed or exceeds the checkpoint limit")
	}
	mediaType := mime.TypeByExtension(ext)
	if mediaType == "" {
		buffer := make([]byte, 512)
		file, _ := os.Open(dest)
		n, _ := file.Read(buffer)
		_ = file.Close()
		mediaType = http.DetectContentType(buffer[:n])
	}
	a := model.Artifact{ID: artifactID, CheckpointID: checkpointID, Name: cleanName, MediaType: mediaType, StoragePath: dest, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: time.Now().UTC()}
	if err := s.Store.CreateArtifact(ctx, a); err != nil {
		_ = os.Remove(dest)
		return model.Artifact{}, err
	}
	s.Bus.Publish("artifact.created", artifactID)
	return a, nil
}

func (s *Service) PrepareIntegration(ctx context.Context, taskID, checkpointID, commitMessage string) (model.IntegrationCandidate, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return model.IntegrationCandidate{}, err
	}
	if checkpointID == "" {
		checkpointID = task.SelectedCheckpointID
	}
	if checkpointID == "" {
		return model.IntegrationCandidate{}, errors.New("select or pass a checkpoint before preparing integration")
	}
	cp, err := s.Store.GetCheckpoint(ctx, checkpointID)
	if err != nil || cp.TaskID != task.ID {
		return model.IntegrationCandidate{}, errors.New("checkpoint does not belong to the task")
	}
	baseOID, err := s.Git.Resolve(ctx, s.Repo, task.BaseRef)
	if err != nil {
		return model.IntegrationCandidate{}, err
	}
	if commitMessage == "" {
		commitMessage = buildCommitMessage(task, cp)
	}
	now := time.Now().UTC()
	candidateID := id.New("candidate")
	branch := fmt.Sprintf("ooh/integration/%s", id.Short(candidateID))
	worktree := filepath.Join(s.WorkspaceRoot, id.Short(task.ID), "integration-"+id.Short(candidateID))
	candidate := model.IntegrationCandidate{ID: candidateID, TaskID: task.ID, CheckpointID: cp.ID, BaseRef: task.BaseRef, BaseOID: baseOID, ChosenOID: cp.CommitOID, WorktreePath: worktree, BranchRef: "refs/heads/" + branch, CommitMessage: commitMessage, State: model.CandidatePreparing, CreatedAt: now, UpdatedAt: now}
	err = s.withLock(func() error {
		if err := s.Git.AddWorktree(ctx, s.Repo, worktree, branch, baseOID); err != nil {
			return err
		}
		if err := s.Store.CreateCandidate(ctx, candidate); err != nil {
			_ = s.Git.RemoveWorktree(ctx, s.Repo, worktree)
			_ = s.Git.DeleteBranch(ctx, s.Repo, candidate.BranchRef)
			return err
		}
		conflict, err := s.Git.MergeSquash(ctx, worktree, cp.CommitOID)
		if err != nil {
			candidate.State, candidate.Error = model.CandidateFailed, err.Error()
			_ = s.Store.UpdateCandidate(ctx, candidate)
			return err
		}
		if conflict {
			candidate.State = model.CandidateNeedsResolution
			task.State = model.TaskNeedsResolution
			_ = s.Store.UpdateTask(ctx, task)
			return s.Store.UpdateCandidate(ctx, candidate)
		}
		dirty, err := s.Git.IndexDirty(ctx, worktree)
		if err != nil {
			return err
		}
		if !dirty {
			candidate.State, candidate.Error = model.CandidateFailed, "the selected changes are already present on the base"
			_ = s.Store.UpdateCandidate(ctx, candidate)
			return errors.New(candidate.Error)
		}
		oid, err := s.Git.Commit(ctx, worktree, commitMessage, false)
		if err != nil {
			candidate.State, candidate.Error = model.CandidateFailed, err.Error()
			_ = s.Store.UpdateCandidate(ctx, candidate)
			return err
		}
		candidate.CommitOID, candidate.State = oid, model.CandidateReady
		return s.Store.UpdateCandidate(ctx, candidate)
	})
	if err != nil {
		return candidate, err
	}
	s.Bus.Publish("integration.prepared", candidate.ID)
	return s.Store.GetCandidate(ctx, candidate.ID)
}

func buildCommitMessage(task model.Task, cp model.Checkpoint) string {
	var b strings.Builder
	b.WriteString(task.Title)
	if cp.Summary != "" {
		b.WriteString("\n\n")
		b.WriteString(cp.Summary)
	}
	if cp.VerificationNotes != "" {
		fmt.Fprintf(&b, "\n\nVerification (%s): %s", cp.VerificationStatus, cp.VerificationNotes)
	}
	if cp.RiskNotes != "" {
		b.WriteString("\n\nRisks: ")
		b.WriteString(cp.RiskNotes)
	}
	b.WriteString("\n\nOoh-Task: ")
	b.WriteString(task.ID)
	return b.String()
}

func (s *Service) ResolveCandidate(ctx context.Context, candidateID string) (model.IntegrationCandidate, error) {
	candidate, err := s.Store.GetCandidate(ctx, candidateID)
	if err != nil {
		return model.IntegrationCandidate{}, err
	}
	if candidate.State != model.CandidateNeedsResolution {
		return candidate, errors.New("candidate is not waiting for conflict resolution")
	}
	err = s.withLock(func() error {
		conflicts, err := s.Git.HasConflicts(ctx, candidate.WorktreePath)
		if err != nil {
			return err
		}
		if conflicts {
			return errors.New("conflicts remain in the resolution workspace")
		}
		if err := s.Git.Run(ctx, candidate.WorktreePath, "add", "-A"); err != nil {
			return err
		}
		oid, err := s.Git.Commit(ctx, candidate.WorktreePath, candidate.CommitMessage, false)
		if err != nil {
			return err
		}
		candidate.CommitOID, candidate.State, candidate.Error = oid, model.CandidateReady, ""
		return s.Store.UpdateCandidate(ctx, candidate)
	})
	if err != nil {
		return candidate, err
	}
	s.Bus.Publish("integration.resolved", candidate.ID)
	return s.Store.GetCandidate(ctx, candidate.ID)
}

func (s *Service) CandidateDiff(ctx context.Context, candidateID, file string) (DiffResult, error) {
	c, err := s.Store.GetCandidate(ctx, candidateID)
	if err != nil {
		return DiffResult{}, err
	}
	if c.CommitOID == "" {
		return DiffResult{}, errors.New("resolve the candidate before previewing its final diff")
	}
	files, err := s.Git.ChangedFiles(ctx, s.Repo, c.BaseOID, c.CommitOID)
	if err != nil {
		return DiffResult{}, err
	}
	patch, err := s.Git.Diff(ctx, s.Repo, c.BaseOID, c.CommitOID, file)
	return DiffResult{BaselineOID: c.BaseOID, TargetOID: c.CommitOID, Files: files, Patch: patch}, err
}

func (s *Service) ApplyIntegration(ctx context.Context, candidateID, confirmOID string) (model.Integration, error) {
	candidate, err := s.Store.GetCandidate(ctx, candidateID)
	if err != nil {
		return model.Integration{}, err
	}
	if candidate.State != model.CandidateReady || candidate.CommitOID == "" {
		return model.Integration{}, errors.New("candidate is not ready to apply")
	}
	if confirmOID != candidate.CommitOID {
		return model.Integration{}, errors.New("confirmation commit does not match the prepared candidate")
	}
	task, err := s.Store.GetTask(ctx, candidate.TaskID)
	if err != nil {
		return model.Integration{}, err
	}
	var integration model.Integration
	err = s.withLock(func() error {
		currentBase, err := s.Git.Resolve(ctx, s.Repo, candidate.BaseRef)
		if err != nil {
			return err
		}
		if currentBase != candidate.BaseOID {
			candidate.State, candidate.Error = model.CandidateStale, "the base branch advanced"
			_ = s.Store.UpdateCandidate(ctx, candidate)
			return errors.New("the base branch advanced; prepare a new candidate")
		}
		if err := s.Git.ValidateFastForward(ctx, s.Repo, candidate.BaseRef, candidate.BaseOID); err != nil {
			return err
		}
		paths, err := s.Store.ListPaths(ctx, task.ID)
		if err != nil {
			return err
		}
		for _, p := range paths {
			if p.State != model.PathArchived && p.State != model.PathDeleted {
				if err := s.archivePathUnlocked(ctx, p); err != nil {
					return fmt.Errorf("archive task path before integration: %w", err)
				}
			}
		}
		integrationID := id.New("integration")
		if err := s.Git.UpdateRef(ctx, s.Repo, "refs/ooh/undo/"+integrationID, candidate.BaseOID); err != nil {
			return err
		}
		if err := s.Git.FastForward(ctx, s.Repo, candidate.BaseRef, candidate.BaseOID, candidate.CommitOID); err != nil {
			return err
		}
		integration = model.Integration{ID: integrationID, TaskID: task.ID, CandidateID: candidate.ID, CheckpointID: candidate.CheckpointID, BaseRef: candidate.BaseRef, BeforeOID: candidate.BaseOID, CommitOID: candidate.CommitOID, CreatedAt: time.Now().UTC()}
		if err := s.Store.CreateIntegration(ctx, integration); err != nil {
			return fmt.Errorf("base was integrated but the recovery record could not be saved: %w", err)
		}
		candidate.State = model.CandidateApplied
		_ = s.Store.UpdateCandidate(ctx, candidate)
		task.State = model.TaskDone
		if err := s.Store.UpdateTask(ctx, task); err != nil {
			return err
		}
		_ = s.Git.RemoveWorktree(ctx, s.Repo, candidate.WorktreePath)
		_ = s.Git.DeleteBranch(ctx, s.Repo, candidate.BranchRef)
		return s.Store.Audit(ctx, "integration.applied", integration.ID, candidate.CommitOID)
	})
	if err != nil {
		return model.Integration{}, err
	}
	s.Bus.Publish("integration.applied", integration.ID)
	return integration, nil
}

func (s *Service) UndoIntegration(ctx context.Context, integrationID string) (model.Integration, error) {
	integration, err := s.Store.GetIntegration(ctx, integrationID)
	if err != nil {
		return model.Integration{}, err
	}
	if integration.UndoneAt != nil {
		return integration, errors.New("integration was already undone")
	}
	err = s.withLock(func() error {
		if err := s.Git.ResetHard(ctx, s.Repo, integration.BaseRef, integration.CommitOID, integration.BeforeOID); err != nil {
			return err
		}
		now := time.Now().UTC()
		integration.UndoneAt = &now
		if err := s.Store.MarkIntegrationUndone(ctx, integration.ID, now); err != nil {
			return err
		}
		task, err := s.Store.GetTask(ctx, integration.TaskID)
		if err != nil {
			return err
		}
		task.State = model.TaskWorking
		if err := s.Store.UpdateTask(ctx, task); err != nil {
			return err
		}
		if task.SelectedPathID != "" {
			_, _ = s.restorePathUnlocked(ctx, task.SelectedPathID)
		}
		return s.Store.Audit(ctx, "integration.undone", integration.ID, "")
	})
	if err != nil {
		return integration, err
	}
	s.Bus.Publish("integration.undone", integration.ID)
	return integration, nil
}

func (s *Service) restorePathUnlocked(ctx context.Context, pathID string) (model.Path, error) {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil || p.State != model.PathArchived {
		return p, err
	}
	worktree := filepath.Join(s.WorkspaceRoot, id.Short(p.TaskID), id.Short(p.ID)+"-"+id.Slug(p.Name))
	startOID, err := s.Git.Resolve(ctx, s.Repo, p.BranchRef)
	if err != nil {
		return p, err
	}
	if err := s.createPathWorkspace(ctx, worktree, p.BranchRef, startOID, p.ID); err != nil {
		return p, err
	}
	p.WorktreePath, p.State, p.ArchivedAt = worktree, model.PathWorking, nil
	return p, s.Store.UpdatePath(ctx, p)
}

func (s *Service) CleanupExpired(ctx context.Context) ([]string, error) {
	before := time.Now().UTC().AddDate(0, 0, -s.Store.RetentionDays(ctx))
	paths, err := s.Store.ArchivedPathsBefore(ctx, before)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, p := range paths {
		err := s.withLock(func() error {
			checkpoints, err := s.Store.ListPathCheckpoints(ctx, p.ID)
			if err != nil {
				return err
			}
			for _, cp := range checkpoints {
				_ = s.Git.DeleteRef(ctx, s.Repo, "refs/ooh/checkpoints/"+cp.ID)
			}
			_ = s.Git.DeleteBranch(ctx, s.Repo, p.BranchRef)
			return s.Store.DeletePathMetadata(ctx, p.ID)
		})
		if err != nil {
			return deleted, err
		}
		deleted = append(deleted, p.ID)
	}
	if len(deleted) > 0 {
		s.Bus.Publish("archive.cleaned", strings.Join(deleted, ","))
	}
	return deleted, nil
}

func (s *Service) ArchivedPaths(ctx context.Context) ([]model.Path, error) {
	return s.Store.ListArchivedPaths(ctx)
}

func (s *Service) SetPathKeep(ctx context.Context, pathID string, keep bool) (model.Path, error) {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return p, err
	}
	p.Keep = keep
	if err := s.Store.UpdatePath(ctx, p); err != nil {
		return p, err
	}
	s.Bus.Publish("archive.updated", pathID)
	return p, nil
}

func (s *Service) DiagnosticBundle(ctx context.Context, destination string) (string, error) {
	if destination == "" {
		destination = filepath.Join(s.Repo.Root, fmt.Sprintf("ooh-diagnostics-%s.zip", time.Now().Format("20060102-150405")))
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(file)
	writer, err := zw.Create("diagnostics.json")
	if err == nil {
		tasks, _ := s.Store.ListTasks(ctx)
		states := map[model.TaskState]int{}
		for _, task := range tasks {
			states[task.State]++
		}
		payload := map[string]any{"oohSchema": 1, "os": runtime.GOOS, "arch": runtime.GOARCH, "goVersion": runtime.Version(), "taskStateCounts": states, "taskCount": len(tasks), "createdAt": time.Now().UTC()}
		err = json.NewEncoder(writer).Encode(payload)
	}
	closeZipErr := zw.Close()
	closeFileErr := file.Close()
	if err != nil || closeZipErr != nil || closeFileErr != nil {
		_ = os.Remove(destination)
		if err != nil {
			return "", err
		}
		if closeZipErr != nil {
			return "", closeZipErr
		}
		return "", closeFileErr
	}
	return destination, nil
}

func RandomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func SortTasksByState(tasks []model.Task) {
	order := map[model.TaskState]int{model.TaskDraft: 0, model.TaskWorking: 1, model.TaskReady: 2, model.TaskChangesRequested: 3, model.TaskNeedsResolution: 4, model.TaskDone: 5, model.TaskArchived: 6}
	sort.SliceStable(tasks, func(i, j int) bool {
		if order[tasks[i].State] == order[tasks[j].State] {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		}
		return order[tasks[i].State] < order[tasks[j].State]
	})
}
