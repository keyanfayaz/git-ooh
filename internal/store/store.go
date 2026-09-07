package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/keyanfayaz/git-ooh/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  base_ref TEXT NOT NULL,
  base_oid TEXT NOT NULL,
  base_tip_oid TEXT NOT NULL,
  state TEXT NOT NULL,
  selected_path_id TEXT NOT NULL DEFAULT '',
  selected_checkpoint_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS paths (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  branch_ref TEXT NOT NULL UNIQUE,
  worktree_path TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  latest_checkpoint_id TEXT NOT NULL DEFAULT '',
  archived_at TEXT,
  keep INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS paths_task_idx ON paths(task_id);
CREATE TABLE IF NOT EXISTS checkpoints (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  path_id TEXT NOT NULL REFERENCES paths(id) ON DELETE CASCADE,
  parent_checkpoint_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  commit_oid TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  verification_status TEXT NOT NULL DEFAULT 'not-run',
  verification_notes TEXT NOT NULL DEFAULT '',
  risk_notes TEXT NOT NULL DEFAULT '',
  ready INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS checkpoints_task_idx ON checkpoints(task_id, created_at);
CREATE INDEX IF NOT EXISTS checkpoints_path_idx ON checkpoints(path_id, created_at);
CREATE TABLE IF NOT EXISTS forks (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  checkpoint_id TEXT NOT NULL REFERENCES checkpoints(id),
  selected_path_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS fork_paths (
  fork_id TEXT NOT NULL REFERENCES forks(id) ON DELETE CASCADE,
  path_id TEXT NOT NULL REFERENCES paths(id) ON DELETE CASCADE,
  ordinal INTEGER NOT NULL,
  PRIMARY KEY (fork_id, path_id)
);
CREATE TABLE IF NOT EXISTS comments (
  id TEXT PRIMARY KEY,
  checkpoint_id TEXT NOT NULL REFERENCES checkpoints(id) ON DELETE CASCADE,
  file_path TEXT NOT NULL DEFAULT '',
  side TEXT NOT NULL DEFAULT '',
  start_line INTEGER NOT NULL DEFAULT 0,
  end_line INTEGER NOT NULL DEFAULT 0,
  body TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  resolved_at TEXT
);
CREATE INDEX IF NOT EXISTS comments_checkpoint_idx ON comments(checkpoint_id, created_at);
CREATE TABLE IF NOT EXISTS artifacts (
  id TEXT PRIMARY KEY,
  checkpoint_id TEXT NOT NULL REFERENCES checkpoints(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  media_type TEXT NOT NULL,
  storage_path TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS integration_candidates (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  checkpoint_id TEXT NOT NULL REFERENCES checkpoints(id),
  base_ref TEXT NOT NULL,
  base_oid TEXT NOT NULL,
  chosen_oid TEXT NOT NULL,
  commit_oid TEXT NOT NULL DEFAULT '',
  worktree_path TEXT NOT NULL,
  branch_ref TEXT NOT NULL,
  commit_message TEXT NOT NULL,
  state TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS integrations (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  candidate_id TEXT NOT NULL REFERENCES integration_candidates(id),
  checkpoint_id TEXT NOT NULL REFERENCES checkpoints(id),
  base_ref TEXT NOT NULL,
  before_oid TEXT NOT NULL,
  commit_oid TEXT NOT NULL,
  created_at TEXT NOT NULL,
  undone_at TEXT
);
CREATE TABLE IF NOT EXISTS agent_runs (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  path_id TEXT NOT NULL REFERENCES paths(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  provider_session_id TEXT NOT NULL DEFAULT '',
  prompt TEXT NOT NULL,
  state TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  verification_status TEXT NOT NULL DEFAULT '',
  verification_notes TEXT NOT NULL DEFAULT '',
  risk_notes TEXT NOT NULL DEFAULT '',
  exit_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT
);
CREATE INDEX IF NOT EXISTS agent_runs_task_idx ON agent_runs(task_id, created_at);
CREATE INDEX IF NOT EXISTS agent_runs_path_idx ON agent_runs(path_id, created_at);
CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate state database: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE agent_runs ADD COLUMN model TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("add agent model to state database: %w", err)
	}
	return nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return value, err
}

func (s *Store) RetentionDays(ctx context.Context) int {
	v, err := s.Setting(ctx, "retention_days")
	if err != nil {
		return 30
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 30
	}
	return n
}

func (s *Store) CreateTaskWithPath(ctx context.Context, task model.Task, path model.Path) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,title,description,base_ref,base_oid,base_tip_oid,state,selected_path_id,selected_checkpoint_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, task.ID, task.Title, task.Description, task.BaseRef, task.BaseOID, task.BaseTipOID, task.State, task.SelectedPathID, task.SelectedCheckpointID, formatTime(task.CreatedAt), formatTime(task.UpdatedAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paths(id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, path.ID, path.TaskID, path.Name, path.BranchRef, path.WorktreePath, path.State, path.LatestCheckpointID, nullableTime(path.ArchivedAt), path.Keep, formatTime(path.CreatedAt), formatTime(path.UpdatedAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET selected_path_id=? WHERE id=?`, path.ID, task.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreatePath(ctx context.Context, p model.Path) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO paths(id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.TaskID, p.Name, p.BranchRef, p.WorktreePath, p.State, p.LatestCheckpointID, nullableTime(p.ArchivedAt), p.Keep, formatTime(p.CreatedAt), formatTime(p.UpdatedAt))
	return err
}

func (s *Store) ListTasks(ctx context.Context) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,title,description,base_ref,base_oid,base_tip_oid,state,selected_path_id,selected_checkpoint_id,created_at,updated_at FROM tasks ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (model.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT id,title,description,base_ref,base_oid,base_tip_oid,state,selected_path_id,selected_checkpoint_id,created_at,updated_at FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, ErrNotFound
	}
	return t, err
}

func (s *Store) UpdateTask(ctx context.Context, t model.Task) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET title=?,description=?,state=?,selected_path_id=?,selected_checkpoint_id=?,updated_at=? WHERE id=?`, t.Title, t.Description, t.State, t.SelectedPathID, t.SelectedCheckpointID, formatTime(t.UpdatedAt), t.ID)
	return err
}

func (s *Store) DeleteTask(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, id)
	return err
}

func (s *Store) GetPath(ctx context.Context, id string) (model.Path, error) {
	p, err := scanPath(s.db.QueryRowContext(ctx, `SELECT id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at FROM paths WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Path{}, ErrNotFound
	}
	return p, err
}

func (s *Store) PathByWorktree(ctx context.Context, worktree string) (model.Path, error) {
	p, err := scanPath(s.db.QueryRowContext(ctx, `SELECT id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at FROM paths WHERE worktree_path=?`, worktree))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Path{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ListPaths(ctx context.Context, taskID string) ([]model.Path, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at FROM paths WHERE task_id=? AND state<>? ORDER BY created_at`, taskID, model.PathDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Path, 0)
	for rows.Next() {
		p, err := scanPath(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePath(ctx context.Context, p model.Path) error {
	p.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE paths SET name=?,worktree_path=?,state=?,latest_checkpoint_id=?,archived_at=?,keep=?,updated_at=? WHERE id=?`, p.Name, p.WorktreePath, p.State, p.LatestCheckpointID, nullableTime(p.ArchivedAt), p.Keep, formatTime(p.UpdatedAt), p.ID)
	return err
}

func (s *Store) CreateAgentRun(ctx context.Context, run model.AgentRun) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_runs(id,task_id,path_id,provider,model,provider_session_id,prompt,state,summary,verification_status,verification_notes,risk_notes,exit_code,error,created_at,started_at,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.TaskID, run.PathID, run.Provider, run.Model, run.ProviderSessionID, run.Prompt, run.State, run.Summary, run.VerificationStatus, run.VerificationNotes, run.RiskNotes, run.ExitCode, run.Error, formatTime(run.CreatedAt), nullableTime(run.StartedAt), nullableTime(run.FinishedAt))
	return err
}

func (s *Store) UpdateAgentRun(ctx context.Context, run model.AgentRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_runs SET provider_session_id=?,state=?,summary=?,verification_status=?,verification_notes=?,risk_notes=?,exit_code=?,error=?,started_at=?,finished_at=? WHERE id=?`, run.ProviderSessionID, run.State, run.Summary, run.VerificationStatus, run.VerificationNotes, run.RiskNotes, run.ExitCode, run.Error, nullableTime(run.StartedAt), nullableTime(run.FinishedAt), run.ID)
	return err
}

func (s *Store) GetAgentRun(ctx context.Context, runID string) (model.AgentRun, error) {
	run, err := scanAgentRun(s.db.QueryRowContext(ctx, `SELECT id,task_id,path_id,provider,model,provider_session_id,prompt,state,summary,verification_status,verification_notes,risk_notes,exit_code,error,created_at,started_at,finished_at FROM agent_runs WHERE id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AgentRun{}, ErrNotFound
	}
	return run, err
}

func (s *Store) ListAgentRuns(ctx context.Context, taskID string) ([]model.AgentRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,path_id,provider,model,provider_session_id,prompt,state,summary,verification_status,verification_notes,risk_notes,exit_code,error,created_at,started_at,finished_at FROM agent_runs WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.AgentRun, 0)
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) LatestAgentRunForPath(ctx context.Context, pathID string) (model.AgentRun, error) {
	run, err := scanAgentRun(s.db.QueryRowContext(ctx, `SELECT id,task_id,path_id,provider,model,provider_session_id,prompt,state,summary,verification_status,verification_notes,risk_notes,exit_code,error,created_at,started_at,finished_at FROM agent_runs WHERE path_id=? ORDER BY created_at DESC LIMIT 1`, pathID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AgentRun{}, ErrNotFound
	}
	return run, err
}

func (s *Store) InterruptActiveRuns(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE agent_runs SET state=?,error='Ooh stopped while this agent was working.',finished_at=? WHERE state IN (?,?)`, model.RunInterrupted, now, model.RunQueued, model.RunRunning)
	return err
}

func (s *Store) CreateCheckpoint(ctx context.Context, cp model.Checkpoint) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO checkpoints(id,task_id,path_id,parent_checkpoint_id,name,commit_oid,summary,verification_status,verification_notes,risk_notes,ready,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, cp.ID, cp.TaskID, cp.PathID, cp.ParentCheckpointID, cp.Name, cp.CommitOID, cp.Summary, cp.VerificationStatus, cp.VerificationNotes, cp.RiskNotes, cp.Ready, formatTime(cp.CreatedAt)); err != nil {
		return err
	}
	pathState := model.PathWorking
	taskState := model.TaskWorking
	if cp.Ready {
		pathState = model.PathReady
		taskState = model.TaskReady
	}
	if _, err = tx.ExecContext(ctx, `UPDATE paths SET latest_checkpoint_id=?,state=?,updated_at=? WHERE id=?`, cp.ID, pathState, formatTime(cp.CreatedAt), cp.PathID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET state=?,selected_path_id=?,selected_checkpoint_id=?,updated_at=? WHERE id=?`, taskState, cp.PathID, cp.ID, formatTime(cp.CreatedAt), cp.TaskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetCheckpoint(ctx context.Context, id string) (model.Checkpoint, error) {
	cp, err := scanCheckpoint(s.db.QueryRowContext(ctx, `SELECT id,task_id,path_id,parent_checkpoint_id,name,commit_oid,summary,verification_status,verification_notes,risk_notes,ready,created_at FROM checkpoints WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Checkpoint{}, ErrNotFound
	}
	return cp, err
}

func (s *Store) ListCheckpoints(ctx context.Context, taskID string) ([]model.Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,path_id,parent_checkpoint_id,name,commit_oid,summary,verification_status,verification_notes,risk_notes,ready,created_at FROM checkpoints WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Checkpoint, 0)
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) ListPathCheckpoints(ctx context.Context, pathID string) ([]model.Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,path_id,parent_checkpoint_id,name,commit_oid,summary,verification_status,verification_notes,risk_notes,ready,created_at FROM checkpoints WHERE path_id=? ORDER BY created_at`, pathID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Checkpoint, 0)
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) CreateFork(ctx context.Context, f model.Fork, pathIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO forks(id,task_id,checkpoint_id,selected_path_id,created_at) VALUES(?,?,?,?,?)`, f.ID, f.TaskID, f.CheckpointID, f.SelectedPathID, formatTime(f.CreatedAt)); err != nil {
		return err
	}
	for i, id := range pathIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO fork_paths(fork_id,path_id,ordinal) VALUES(?,?,?)`, f.ID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListForks(ctx context.Context, taskID string) ([]model.Fork, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,checkpoint_id,selected_path_id,created_at FROM forks WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Fork, 0)
	for rows.Next() {
		var f model.Fork
		var created string
		if err := rows.Scan(&f.ID, &f.TaskID, &f.CheckpointID, &f.SelectedPathID, &created); err != nil {
			return nil, err
		}
		f.CreatedAt = parseTime(created)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) SiblingPaths(ctx context.Context, pathID string) (model.Fork, []model.Path, error) {
	var f model.Fork
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT f.id,f.task_id,f.checkpoint_id,f.selected_path_id,f.created_at FROM forks f JOIN fork_paths fp ON fp.fork_id=f.id WHERE fp.path_id=? ORDER BY f.created_at DESC LIMIT 1`, pathID).Scan(&f.ID, &f.TaskID, &f.CheckpointID, &f.SelectedPathID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Fork{}, nil, ErrNotFound
	}
	if err != nil {
		return model.Fork{}, nil, err
	}
	f.CreatedAt = parseTime(created)
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.task_id,p.name,p.branch_ref,p.worktree_path,p.state,p.latest_checkpoint_id,p.archived_at,p.keep,p.created_at,p.updated_at FROM paths p JOIN fork_paths fp ON fp.path_id=p.id WHERE fp.fork_id=? ORDER BY fp.ordinal`, f.ID)
	if err != nil {
		return model.Fork{}, nil, err
	}
	defer rows.Close()
	paths := make([]model.Path, 0)
	for rows.Next() {
		p, err := scanPath(rows)
		if err != nil {
			return model.Fork{}, nil, err
		}
		paths = append(paths, p)
	}
	return f, paths, rows.Err()
}

func (s *Store) SelectForkPath(ctx context.Context, forkID, pathID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE forks SET selected_path_id=? WHERE id=?`, pathID, forkID)
	return err
}

func (s *Store) CreateComment(ctx context.Context, c model.Comment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO comments(id,checkpoint_id,file_path,side,start_line,end_line,body,status,created_at,resolved_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, c.ID, c.CheckpointID, c.FilePath, c.Side, c.StartLine, c.EndLine, c.Body, c.Status, formatTime(c.CreatedAt), nullableTime(c.ResolvedAt))
	return err
}

func (s *Store) ResolveComment(ctx context.Context, id string, resolved bool) error {
	status := model.CommentOpen
	var at any
	if resolved {
		status = model.CommentResolved
		at = formatTime(time.Now().UTC())
	}
	res, err := s.db.ExecContext(ctx, `UPDATE comments SET status=?,resolved_at=? WHERE id=?`, status, at, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListComments(ctx context.Context, checkpointID string) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,checkpoint_id,file_path,side,start_line,end_line,body,status,created_at,resolved_at FROM comments WHERE checkpoint_id=? ORDER BY created_at`, checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectComments(rows)
}

func (s *Store) ListTaskComments(ctx context.Context, taskID string) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.checkpoint_id,c.file_path,c.side,c.start_line,c.end_line,c.body,c.status,c.created_at,c.resolved_at FROM comments c JOIN checkpoints cp ON cp.id=c.checkpoint_id WHERE cp.task_id=? ORDER BY c.created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectComments(rows)
}

func (s *Store) CreateArtifact(ctx context.Context, a model.Artifact) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(id,checkpoint_id,name,media_type,storage_path,size,sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, a.CheckpointID, a.Name, a.MediaType, a.StoragePath, a.Size, a.SHA256, formatTime(a.CreatedAt))
	return err
}

func (s *Store) GetArtifact(ctx context.Context, id string) (model.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, `SELECT id,checkpoint_id,name,media_type,storage_path,size,sha256,created_at FROM artifacts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Artifact{}, ErrNotFound
	}
	return a, err
}

func (s *Store) ListTaskArtifacts(ctx context.Context, taskID string) ([]model.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.checkpoint_id,a.name,a.media_type,a.storage_path,a.size,a.sha256,a.created_at FROM artifacts a JOIN checkpoints cp ON cp.id=a.checkpoint_id WHERE cp.task_id=? ORDER BY a.created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Artifact, 0)
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListCheckpointArtifacts(ctx context.Context, checkpointID string) ([]model.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,checkpoint_id,name,media_type,storage_path,size,sha256,created_at FROM artifacts WHERE checkpoint_id=? ORDER BY created_at`, checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Artifact, 0)
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CreateCandidate(ctx context.Context, c model.IntegrationCandidate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO integration_candidates(id,task_id,checkpoint_id,base_ref,base_oid,chosen_oid,commit_oid,worktree_path,branch_ref,commit_message,state,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.TaskID, c.CheckpointID, c.BaseRef, c.BaseOID, c.ChosenOID, c.CommitOID, c.WorktreePath, c.BranchRef, c.CommitMessage, c.State, c.Error, formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	return err
}

func (s *Store) UpdateCandidate(ctx context.Context, c model.IntegrationCandidate) error {
	c.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE integration_candidates SET commit_oid=?,state=?,error=?,updated_at=? WHERE id=?`, c.CommitOID, c.State, c.Error, formatTime(c.UpdatedAt), c.ID)
	return err
}

func (s *Store) GetCandidate(ctx context.Context, id string) (model.IntegrationCandidate, error) {
	c, err := scanCandidate(s.db.QueryRowContext(ctx, `SELECT id,task_id,checkpoint_id,base_ref,base_oid,chosen_oid,commit_oid,worktree_path,branch_ref,commit_message,state,error,created_at,updated_at FROM integration_candidates WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.IntegrationCandidate{}, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateIntegration(ctx context.Context, i model.Integration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO integrations(id,task_id,candidate_id,checkpoint_id,base_ref,before_oid,commit_oid,created_at,undone_at) VALUES(?,?,?,?,?,?,?,?,?)`, i.ID, i.TaskID, i.CandidateID, i.CheckpointID, i.BaseRef, i.BeforeOID, i.CommitOID, formatTime(i.CreatedAt), nullableTime(i.UndoneAt))
	return err
}

func (s *Store) GetIntegration(ctx context.Context, id string) (model.Integration, error) {
	i, err := scanIntegration(s.db.QueryRowContext(ctx, `SELECT id,task_id,candidate_id,checkpoint_id,base_ref,before_oid,commit_oid,created_at,undone_at FROM integrations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Integration{}, ErrNotFound
	}
	return i, err
}

func (s *Store) MarkIntegrationUndone(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE integrations SET undone_at=? WHERE id=?`, formatTime(at), id)
	return err
}

func (s *Store) ArchivedPathsBefore(ctx context.Context, before time.Time) ([]model.Path, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at FROM paths WHERE state=? AND keep=0 AND archived_at IS NOT NULL AND archived_at<?`, model.PathArchived, formatTime(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Path, 0)
	for rows.Next() {
		p, err := scanPath(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListArchivedPaths(ctx context.Context) ([]model.Path, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,name,branch_ref,worktree_path,state,latest_checkpoint_id,archived_at,keep,created_at,updated_at FROM paths WHERE state=? ORDER BY archived_at`, model.PathArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Path, 0)
	for rows.Next() {
		p, err := scanPath(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListForkPaths(ctx context.Context, forkID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path_id FROM fork_paths WHERE fork_id=? ORDER BY ordinal`, forkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pathID string
		if err := rows.Scan(&pathID); err != nil {
			return nil, err
		}
		out = append(out, pathID)
	}
	return out, rows.Err()
}

func (s *Store) DeletePathMetadata(ctx context.Context, pathID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE paths SET state=?,worktree_path='',updated_at=? WHERE id=?`, model.PathDeleted, formatTime(time.Now().UTC()), pathID)
	return err
}

func (s *Store) Audit(ctx context.Context, kind, entityID, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(kind,entity_id,detail,created_at) VALUES(?,?,?,?)`, kind, entityID, detail, formatTime(time.Now().UTC()))
	return err
}

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (model.Task, error) {
	var t model.Task
	var created, updated string
	err := row.Scan(&t.ID, &t.Title, &t.Description, &t.BaseRef, &t.BaseOID, &t.BaseTipOID, &t.State, &t.SelectedPathID, &t.SelectedCheckpointID, &created, &updated)
	t.CreatedAt, t.UpdatedAt = parseTime(created), parseTime(updated)
	return t, err
}

func scanPath(row scanner) (model.Path, error) {
	var p model.Path
	var archived sql.NullString
	var created, updated string
	err := row.Scan(&p.ID, &p.TaskID, &p.Name, &p.BranchRef, &p.WorktreePath, &p.State, &p.LatestCheckpointID, &archived, &p.Keep, &created, &updated)
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	if archived.Valid {
		t := parseTime(archived.String)
		p.ArchivedAt = &t
	}
	return p, err
}

func scanCheckpoint(row scanner) (model.Checkpoint, error) {
	var cp model.Checkpoint
	var created string
	err := row.Scan(&cp.ID, &cp.TaskID, &cp.PathID, &cp.ParentCheckpointID, &cp.Name, &cp.CommitOID, &cp.Summary, &cp.VerificationStatus, &cp.VerificationNotes, &cp.RiskNotes, &cp.Ready, &created)
	cp.CreatedAt = parseTime(created)
	return cp, err
}

func collectComments(rows *sql.Rows) ([]model.Comment, error) {
	out := make([]model.Comment, 0)
	for rows.Next() {
		var c model.Comment
		var created string
		var resolved sql.NullString
		if err := rows.Scan(&c.ID, &c.CheckpointID, &c.FilePath, &c.Side, &c.StartLine, &c.EndLine, &c.Body, &c.Status, &created, &resolved); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(created)
		if resolved.Valid {
			t := parseTime(resolved.String)
			c.ResolvedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanArtifact(row scanner) (model.Artifact, error) {
	var a model.Artifact
	var created string
	err := row.Scan(&a.ID, &a.CheckpointID, &a.Name, &a.MediaType, &a.StoragePath, &a.Size, &a.SHA256, &created)
	a.CreatedAt = parseTime(created)
	return a, err
}

func scanCandidate(row scanner) (model.IntegrationCandidate, error) {
	var c model.IntegrationCandidate
	var created, updated string
	err := row.Scan(&c.ID, &c.TaskID, &c.CheckpointID, &c.BaseRef, &c.BaseOID, &c.ChosenOID, &c.CommitOID, &c.WorktreePath, &c.BranchRef, &c.CommitMessage, &c.State, &c.Error, &created, &updated)
	c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
	return c, err
}

func scanIntegration(row scanner) (model.Integration, error) {
	var i model.Integration
	var created string
	var undone sql.NullString
	err := row.Scan(&i.ID, &i.TaskID, &i.CandidateID, &i.CheckpointID, &i.BaseRef, &i.BeforeOID, &i.CommitOID, &created, &undone)
	i.CreatedAt = parseTime(created)
	if undone.Valid {
		t := parseTime(undone.String)
		i.UndoneAt = &t
	}
	return i, err
}

func scanAgentRun(row scanner) (model.AgentRun, error) {
	var run model.AgentRun
	var created string
	var started, finished sql.NullString
	err := row.Scan(&run.ID, &run.TaskID, &run.PathID, &run.Provider, &run.Model, &run.ProviderSessionID, &run.Prompt, &run.State, &run.Summary, &run.VerificationStatus, &run.VerificationNotes, &run.RiskNotes, &run.ExitCode, &run.Error, &created, &started, &finished)
	run.CreatedAt = parseTime(created)
	if started.Valid {
		t := parseTime(started.String)
		run.StartedAt = &t
	}
	if finished.Valid {
		t := parseTime(finished.String)
		run.FinishedAt = &t
	}
	return run, err
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}
