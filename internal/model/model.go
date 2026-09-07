package model

import "time"

type TaskState string

const (
	TaskDraft            TaskState = "draft"
	TaskWorking          TaskState = "working"
	TaskReady            TaskState = "ready"
	TaskChangesRequested TaskState = "changes_requested"
	TaskNeedsResolution  TaskState = "needs_resolution"
	TaskDone             TaskState = "done"
	TaskArchived         TaskState = "archived"
)

type RunState string

const (
	RunQueued      RunState = "queued"
	RunRunning     RunState = "running"
	RunSucceeded   RunState = "succeeded"
	RunFailed      RunState = "failed"
	RunStopped     RunState = "stopped"
	RunInterrupted RunState = "interrupted"
)

type PathState string

const (
	PathWorking          PathState = "working"
	PathReady            PathState = "ready"
	PathChangesRequested PathState = "changes_requested"
	PathSelected         PathState = "selected"
	PathArchived         PathState = "archived"
	PathDeleted          PathState = "deleted"
)

type CommentStatus string

const (
	CommentOpen     CommentStatus = "open"
	CommentResolved CommentStatus = "resolved"
)

type CandidateState string

const (
	CandidatePreparing       CandidateState = "preparing"
	CandidateNeedsResolution CandidateState = "needs_resolution"
	CandidateReady           CandidateState = "ready"
	CandidateApplied         CandidateState = "applied"
	CandidateStale           CandidateState = "stale"
	CandidateFailed          CandidateState = "failed"
)

type Task struct {
	ID                   string    `json:"id"`
	Title                string    `json:"title"`
	Description          string    `json:"description"`
	BaseRef              string    `json:"baseRef"`
	BaseOID              string    `json:"baseOid"`
	BaseTipOID           string    `json:"baseTipOid"`
	State                TaskState `json:"state"`
	SelectedPathID       string    `json:"selectedPathId,omitempty"`
	SelectedCheckpointID string    `json:"selectedCheckpointId,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
	ActiveRuns           int       `json:"activeRuns,omitempty"`
}

type Path struct {
	ID                 string     `json:"id"`
	TaskID             string     `json:"taskId"`
	Name               string     `json:"name"`
	BranchRef          string     `json:"branchRef"`
	WorktreePath       string     `json:"worktreePath,omitempty"`
	State              PathState  `json:"state"`
	LatestCheckpointID string     `json:"latestCheckpointId,omitempty"`
	ArchivedAt         *time.Time `json:"archivedAt,omitempty"`
	Keep               bool       `json:"keep"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

type Fork struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"taskId"`
	CheckpointID   string    `json:"checkpointId"`
	SelectedPathID string    `json:"selectedPathId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Checkpoint struct {
	ID                 string    `json:"id"`
	TaskID             string    `json:"taskId"`
	PathID             string    `json:"pathId"`
	ParentCheckpointID string    `json:"parentCheckpointId,omitempty"`
	Name               string    `json:"name"`
	CommitOID          string    `json:"commitOid"`
	Summary            string    `json:"summary"`
	VerificationStatus string    `json:"verificationStatus"`
	VerificationNotes  string    `json:"verificationNotes"`
	RiskNotes          string    `json:"riskNotes,omitempty"`
	Ready              bool      `json:"ready"`
	CreatedAt          time.Time `json:"createdAt"`
}

type Comment struct {
	ID           string        `json:"id"`
	CheckpointID string        `json:"checkpointId"`
	FilePath     string        `json:"filePath,omitempty"`
	Side         string        `json:"side,omitempty"`
	StartLine    int           `json:"startLine,omitempty"`
	EndLine      int           `json:"endLine,omitempty"`
	Body         string        `json:"body"`
	Status       CommentStatus `json:"status"`
	CreatedAt    time.Time     `json:"createdAt"`
	ResolvedAt   *time.Time    `json:"resolvedAt,omitempty"`
}

type Artifact struct {
	ID           string    `json:"id"`
	CheckpointID string    `json:"checkpointId"`
	Name         string    `json:"name"`
	MediaType    string    `json:"mediaType"`
	StoragePath  string    `json:"-"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	CreatedAt    time.Time `json:"createdAt"`
}

type IntegrationCandidate struct {
	ID            string         `json:"id"`
	TaskID        string         `json:"taskId"`
	CheckpointID  string         `json:"checkpointId"`
	BaseRef       string         `json:"baseRef"`
	BaseOID       string         `json:"baseOid"`
	ChosenOID     string         `json:"chosenOid"`
	CommitOID     string         `json:"commitOid,omitempty"`
	WorktreePath  string         `json:"worktreePath"`
	BranchRef     string         `json:"branchRef"`
	CommitMessage string         `json:"commitMessage"`
	State         CandidateState `json:"state"`
	Error         string         `json:"error,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

type Integration struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"taskId"`
	CandidateID  string     `json:"candidateId"`
	CheckpointID string     `json:"checkpointId"`
	BaseRef      string     `json:"baseRef"`
	BeforeOID    string     `json:"beforeOid"`
	CommitOID    string     `json:"commitOid"`
	CreatedAt    time.Time  `json:"createdAt"`
	UndoneAt     *time.Time `json:"undoneAt,omitempty"`
}

type AgentRun struct {
	ID                 string     `json:"id"`
	TaskID             string     `json:"taskId"`
	PathID             string     `json:"pathId"`
	Provider           string     `json:"provider"`
	Model              string     `json:"model,omitempty"`
	ProviderSessionID  string     `json:"providerSessionId,omitempty"`
	Prompt             string     `json:"prompt"`
	State              RunState   `json:"state"`
	Summary            string     `json:"summary,omitempty"`
	VerificationStatus string     `json:"verificationStatus,omitempty"`
	VerificationNotes  string     `json:"verificationNotes,omitempty"`
	RiskNotes          string     `json:"riskNotes,omitempty"`
	ExitCode           int        `json:"exitCode,omitempty"`
	Error              string     `json:"error,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	StartedAt          *time.Time `json:"startedAt,omitempty"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
}

type ChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

type WorktreeStatus struct {
	Path      string `json:"path"`
	BranchRef string `json:"branchRef"`
	HeadOID   string `json:"headOid"`
	Dirty     bool   `json:"dirty"`
	Changed   int    `json:"changed"`
	Untracked int    `json:"untracked"`
}

type TaskDetail struct {
	Task        Task                      `json:"task"`
	Paths       []Path                    `json:"paths"`
	Forks       []Fork                    `json:"forks"`
	Checkpoints []Checkpoint              `json:"checkpoints"`
	Comments    []Comment                 `json:"comments"`
	Artifacts   []Artifact                `json:"artifacts"`
	Statuses    map[string]WorktreeStatus `json:"statuses"`
	Runs        []AgentRun                `json:"runs"`
}
