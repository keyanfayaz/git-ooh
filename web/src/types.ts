export type TaskState = 'draft' | 'working' | 'ready' | 'changes_requested' | 'needs_resolution' | 'done' | 'archived'
export type PathState = 'working' | 'ready' | 'changes_requested' | 'selected' | 'archived' | 'deleted'

export interface Task {
  id: string
  title: string
  description: string
  baseRef: string
  baseOid: string
  baseTipOid: string
  state: TaskState
  selectedPathId?: string
  selectedCheckpointId?: string
  createdAt: string
  updatedAt: string
  activeRuns?: number
}

export interface Path {
  id: string
  taskId: string
  name: string
  branchRef: string
  worktreePath?: string
  state: PathState
  latestCheckpointId?: string
  archivedAt?: string
  keep: boolean
  createdAt: string
  updatedAt: string
}

export interface Checkpoint {
  id: string
  taskId: string
  pathId: string
  parentCheckpointId?: string
  name: string
  commitOid: string
  summary: string
  verificationStatus: string
  verificationNotes: string
  riskNotes?: string
  ready: boolean
  createdAt: string
}

export interface Comment {
  id: string
  checkpointId: string
  filePath?: string
  side?: string
  startLine?: number
  endLine?: number
  body: string
  status: 'open' | 'resolved'
  createdAt: string
}

export interface Artifact {
  id: string
  checkpointId: string
  name: string
  mediaType: string
  size: number
  sha256: string
  createdAt: string
}

export interface WorktreeStatus {
  path: string
  branchRef: string
  headOid: string
  dirty: boolean
  changed: number
  untracked: number
}

export interface TaskDetail {
  task: Task
  paths: Path[]
  checkpoints: Checkpoint[]
  comments: Comment[]
  artifacts: Artifact[]
  statuses: Record<string, WorktreeStatus>
  runs: AgentRun[]
}

export interface AgentRun {
  id: string
  taskId: string
  pathId: string
  provider: string
  model?: string
  providerSessionId?: string
  prompt: string
  state: 'queued' | 'running' | 'succeeded' | 'failed' | 'stopped' | 'interrupted'
  summary?: string
  verificationStatus?: string
  verificationNotes?: string
  riskNotes?: string
  exitCode?: number
  error?: string
  createdAt: string
  startedAt?: string
  finishedAt?: string
}

export interface AgentInfo {
  id: string
  name: string
  available: boolean
  version?: string
  reason?: string
  models: AgentModel[]
}

export interface AgentModel {
  id: string
  name: string
}

export interface TreeEntry {
  name: string
  path: string
  kind: 'file' | 'directory'
  size?: number
  modified?: string
}

export interface WorkspaceFile {
  path: string
  content: string
  revision: string
  language: string
  mediaType: string
  size: number
}

export interface SearchMatch {
  path: string
  line: number
  preview: string
}

export interface PreviewCapability {
  available: boolean
  kind?: string
  installCommand?: string
  runCommand?: string
  expectedUrl?: string
  requiresApproval: boolean
  reason?: string
}

export interface PreviewSession {
  id: string
  pathId: string
  state: 'starting' | 'running' | 'failed' | 'stopped'
  url?: string
  error?: string
  log?: string
  startedAt: string
}

export interface ChangedFile {
  path: string
  oldPath?: string
  status: string
  additions: number
  deletions: number
  binary: boolean
}

export interface DiffResult {
  baselineOid: string
  targetOid: string
  files: ChangedFile[]
  patch: string
}

export interface Candidate {
  id: string
  taskId: string
  checkpointId: string
  baseRef: string
  baseOid: string
  chosenOid: string
  commitOid?: string
  worktreePath: string
  state: 'preparing' | 'needs_resolution' | 'ready' | 'applied' | 'stale' | 'failed'
  error?: string
}

export interface Integration {
  id: string
  taskId: string
  candidateId: string
  checkpointId: string
  baseRef: string
  beforeOid: string
  commitOid: string
  createdAt: string
  undoneAt?: string
}

export interface Bootstrap {
  repoId: string
  repoName: string
  repoRoot: string
  version: string
  retentionDays: number
}
