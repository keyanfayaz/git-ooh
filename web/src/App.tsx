import { FormEvent, ReactNode, useCallback, useEffect, useMemo, useState } from 'react'
import { Diff, Hunk, parseDiff } from 'react-diff-view'
import { api, post, shortId, shortOid } from './api'
import Workspace from './Workspace'
import type { AgentInfo, Bootstrap, Candidate, Checkpoint, Comment, DiffResult, Integration, Path, Task, TaskDetail } from './types'

const stateLabel: Record<string, string> = {
  draft: 'Draft',
  working: 'In progress',
  ready: 'Ready for review',
  changes_requested: 'Changes requested',
  needs_resolution: 'Needs help',
  done: 'Finished',
  archived: 'Archived',
  selected: 'Selected',
}

const groups = [
  { title: 'In progress', states: ['draft', 'working', 'changes_requested', 'needs_resolution'] },
  { title: 'Ready to review', states: ['ready'] },
  { title: 'Finished', states: ['done'] },
]

type TaskVariation = { name: string; provider: string; model: string; guidance: string }
type TaskSetup = { instructions: string; variations: TaskVariation[] }

function Button({ children, kind = 'secondary', ...props }: React.ButtonHTMLAttributes<HTMLButtonElement> & { kind?: 'primary' | 'secondary' | 'quiet' | 'danger' }) {
  return <button className={`button ${kind}`} {...props}>{children}</button>
}

function Modal({ title, children, onClose, wide = false }: { title: string; children: ReactNode; onClose: () => void; wide?: boolean }) {
  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <section className={`modal ${wide ? 'wide' : ''}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head"><h2>{title}</h2><button className="icon-button" onClick={onClose} aria-label="Close">×</button></div>
        {children}
      </section>
    </div>
  )
}

function TaskSetupModal({ agents, starting, onClose, onStart }: { agents: AgentInfo[]; starting: boolean; onClose: () => void; onStart: (input: TaskSetup) => Promise<void> }) {
  const available = agents.filter((agent) => agent.available)
  const remembered = (typeof window.sessionStorage?.getItem === 'function' ? window.sessionStorage.getItem('ooh-agent') : '') || ''
  const initialProvider = available.some((agent) => agent.id === remembered) ? remembered : available[0]?.id || ''
  const [instructions, setInstructions] = useState('')
  const [variationCount, setVariationCount] = useState(1)
  const [variations, setVariations] = useState<TaskVariation[]>([{ name: 'Approach 1', provider: initialProvider, model: '', guidance: '' }])

  function updateCount(value: number) {
    const next = Number.isFinite(value) ? Math.max(1, Math.floor(value)) : 1
    setVariationCount(next)
    setVariations((current) => Array.from({ length: next }, (_, index) => current[index] || { name: `Approach ${index + 1}`, provider: initialProvider, model: '', guidance: '' }))
  }

  function updateVariation(index: number, update: Partial<TaskVariation>) {
    setVariations((current) => current.map((variation, itemIndex) => itemIndex === index ? { ...variation, ...update } : variation))
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!instructions.trim() || !available.length) return
    if (typeof window.sessionStorage?.setItem === 'function') window.sessionStorage.setItem('ooh-agent', variations[0]?.provider || '')
    await onStart({ instructions: instructions.trim(), variations })
  }

  return <Modal title="Create a task" onClose={onClose} wide>
    <form className="form task-setup-form" onSubmit={submit}>
      <label>What should change?<span>Describe the outcome in everyday language. Each variation starts from the current version.</span><textarea autoFocus value={instructions} onChange={(event) => setInstructions(event.target.value)} placeholder="For example: Simplify the checkout screen and make the next step obvious" required /></label>
      <label className="variation-count">Number of variations<span>Each variation gets its own isolated copy and can use a different agent or model.</span><input type="number" min="1" step="1" value={variationCount} onChange={(event) => updateCount(event.currentTarget.valueAsNumber)} /></label>
      <div className="task-variation-list">{variations.map((variation, index) => {
        const selectedAgent = available.find((agent) => agent.id === variation.provider)
        const models = selectedAgent?.models?.length ? selectedAgent.models : [{ id: '', name: 'Default' }]
        return <section className="task-variation" key={index}>
          <div className="task-variation-title"><strong>Variation {index + 1}</strong><span>Approach {index + 1}</span></div>
          <div className="form-row">
            <label>Coding agent<select aria-label={`Variation ${index + 1} coding agent`} value={variation.provider} onChange={(event) => updateVariation(index, { provider: event.target.value, model: '' })}>{available.map((agent) => <option key={agent.id} value={agent.id}>{agent.name}</option>)}</select></label>
            <label>Model<select aria-label={`Variation ${index + 1} model`} value={variation.model} onChange={(event) => updateVariation(index, { model: event.target.value })}>{models.map((model) => <option key={model.id || 'default'} value={model.id}>{model.name}</option>)}</select></label>
          </div>
          <label>Optional direction<span>Use this only when this variation should try a particular approach.</span><textarea aria-label={`Variation ${index + 1} optional direction`} value={variation.guidance} onChange={(event) => updateVariation(index, { guidance: event.target.value })} placeholder="For example: Keep the implementation as small as possible" /></label>
        </section>
      })}</div>
      {!available.length && <div className="setup-warning">Install and sign in to Codex or Claude Code before starting a task.</div>}
      <p className="setup-permission-note">Agents use their own workspace permission controls. Preview and terminal commands run with your normal local user permissions.</p>
      <div className="modal-actions"><Button type="button" onClick={onClose}>Cancel</Button><Button kind="primary" disabled={starting || !instructions.trim() || !available.length}>{starting ? 'Creating isolated copies…' : `Start ${variationCount} variation${variationCount === 1 ? '' : 's'}`}</Button></div>
    </form>
  </Modal>
}

function Empty({ title, children }: { title: string; children: ReactNode }) {
  return <div className="empty"><div className="empty-mark">⌁</div><h2>{title}</h2><p>{children}</p></div>
}

function TaskCard({ task, onOpen }: { task: Task; onOpen: () => void }) {
  return (
    <button className="task-card" onClick={onOpen}>
      <span className={`status-dot ${task.state} ${task.activeRuns ? 'pulsing' : ''}`} />
      <span className="task-copy"><strong>{task.title}</strong><small>{task.description || `Based on ${task.baseRef.replace('refs/heads/', '')}`}</small></span>
      <span className="chevron">›</span>
    </button>
  )
}

function Overview({ tasks, onOpen, onCreate }: { tasks: Task[]; onOpen: (id: string) => void; onCreate: () => void }) {
  const visible = (tasks || []).filter((task) => task.state !== 'archived')
  return (
    <main className="page overview">
      <div className="page-title">
        <div><p className="eyebrow">Workspace</p><h1>What’s being explored</h1><p>Each task has its own safe workspace. Alternative paths can run side by side.</p></div>
        <Button kind="primary" onClick={onCreate}>New task</Button>
      </div>
      {visible.length === 0 ? (
        <Empty title="No tasks yet">Create a task to give an agent a separate place to work without disturbing your main branch.</Empty>
      ) : (
        <div className="board">
          {groups.map((group) => {
            const items = visible.filter((task) => group.states.includes(task.state))
            return <section className="board-column" key={group.title}><header><h2>{group.title}</h2><span>{items.length}</span></header><div className="task-list">{items.map((task) => <TaskCard key={task.id} task={task} onOpen={() => onOpen(task.id)} />)}{!items.length && <p className="column-empty">Nothing here</p>}</div></section>
          })}
        </div>
      )}
    </main>
  )
}

function PathCard({ path, detail, onRefresh, onReview }: { path: Path; detail: TaskDetail; onRefresh: () => Promise<void>; onReview: (id: string) => void }) {
  const [menu, setMenu] = useState<'checkpoint' | 'ready' | 'fork' | null>(null)
  const status = detail.statuses[path.id]
  const latest = detail.checkpoints.find((checkpoint) => checkpoint.id === path.latestCheckpointId)
  const active = !['archived', 'deleted'].includes(path.state)

  async function submitCheckpoint(event: FormEvent<HTMLFormElement>, ready: boolean) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    await post(`/api/paths/${path.id}/${ready ? 'ready' : 'checkpoint'}`, {
      name: form.get('name'), summary: form.get('summary'),
      verificationStatus: form.get('verificationStatus'), verificationNotes: form.get('verificationNotes'), riskNotes: form.get('riskNotes'),
    })
    setMenu(null)
    await onRefresh()
  }

  async function submitFork(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const names = String(form.get('names') || '').split(',').map((name) => name.trim()).filter(Boolean)
    await post(`/api/paths/${path.id}/fork`, { names, summary: form.get('summary') })
    setMenu(null)
    await onRefresh()
  }

  return (
    <article className={`path-card ${path.state === 'selected' ? 'chosen' : ''}`}>
      <div className="path-top"><div><span className={`pill ${path.state}`}>{stateLabel[path.state] || path.state}</span><h3>{path.name}</h3></div>{status?.dirty && <span className="live-badge">{status.changed} live change{status.changed === 1 ? '' : 's'}</span>}</div>
      <p className="workspace-label">Workspace</p>
      <button className="workspace-path" title="Copy workspace path" onClick={() => navigator.clipboard.writeText(path.worktreePath || '')}>{path.worktreePath || 'Archived'} <span>Copy</span></button>
      <div className="path-meta"><span>{latest ? `Last saved ${new Date(latest.createdAt).toLocaleString()}` : 'No checkpoint yet'}</span><span>{shortOid(status?.headOid)}</span></div>
      <div className="path-actions">
        {latest && <Button kind="quiet" onClick={() => onReview(latest.id)}>Review</Button>}
        {active && <Button kind="quiet" onClick={() => setMenu('checkpoint')}>Save checkpoint</Button>}
        {active && <Button kind="quiet" onClick={() => setMenu('fork')}>Try alternatives</Button>}
        {active && <Button kind="primary" onClick={() => setMenu('ready')}>Mark ready</Button>}
      </div>
      {(menu === 'checkpoint' || menu === 'ready') && <Modal title={menu === 'ready' ? 'Ready for review' : 'Save a checkpoint'} onClose={() => setMenu(null)}><CheckpointForm ready={menu === 'ready'} onSubmit={(event) => submitCheckpoint(event, menu === 'ready')} /></Modal>}
      {menu === 'fork' && <Modal title="Try alternative paths" onClose={() => setMenu(null)}><form className="form" onSubmit={submitFork}><label>Path names<span>Separate two or more ideas with commas.</span><input name="names" placeholder="Simple approach, Thorough approach" required /></label><label>What are we comparing?<textarea name="summary" placeholder="Explore two ways to simplify onboarding" /></label><div className="modal-actions"><Button type="button" onClick={() => setMenu(null)}>Cancel</Button><Button kind="primary">Create paths</Button></div></form></Modal>}
    </article>
  )
}

function CheckpointForm({ ready, onSubmit }: { ready: boolean; onSubmit: (event: FormEvent<HTMLFormElement>) => void }) {
  return <form className="form" onSubmit={onSubmit}><label>Name<input name="name" placeholder={ready ? 'Ready for review' : 'Working checkpoint'} /></label><label>What changed?{ready && <span>Required so reviewers can start with the big picture.</span>}<textarea name="summary" required={ready} placeholder="Describe the result in plain language" /></label><div className="form-row"><label>Checks<select name="verificationStatus" defaultValue="not-run"><option value="passed">Passed</option><option value="failed">Failed</option><option value="not-run">Not run</option></select></label><label>Check notes<input name="verificationNotes" required={ready} placeholder="What was tested, or why not" /></label></div><label>Anything risky?<textarea name="riskNotes" placeholder="Optional risks, tradeoffs, or follow-ups" /></label><div className="modal-actions"><Button kind="primary">{ready ? 'Mark ready' : 'Save checkpoint'}</Button></div></form>
}

function ReviewPanel({ checkpoint, detail, onRefresh }: { checkpoint: Checkpoint; detail: TaskDetail; onRefresh: () => Promise<void> }) {
  const [diff, setDiff] = useState<DiffResult>()
  const [view, setView] = useState<'unified' | 'split'>('unified')
  const [loading, setLoading] = useState(true)
  const [commenting, setCommenting] = useState<string | null>(null)
  const [candidate, setCandidate] = useState<Candidate>()
  const [finalDiff, setFinalDiff] = useState<DiffResult>()
  const [integration, setIntegration] = useState<Integration>()
  const path = detail.paths.find((item) => item.id === checkpoint.pathId)
  const comments = detail.comments.filter((comment) => comment.checkpointId === checkpoint.id)
  const evidence = detail.artifacts.filter((artifact) => artifact.checkpointId === checkpoint.id)

  const loadDiff = useCallback(async () => {
    setLoading(true)
    try { setDiff(await api(`/api/checkpoints/${checkpoint.id}/diff`)) } finally { setLoading(false) }
  }, [checkpoint.id])

  useEffect(() => { void loadDiff(); setCandidate(undefined); setFinalDiff(undefined); setIntegration(undefined) }, [loadDiff])

  async function addComment(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    await post(`/api/checkpoints/${checkpoint.id}/comments`, { filePath: form.get('filePath'), side: form.get('side'), startLine: Number(form.get('startLine') || 0), endLine: Number(form.get('endLine') || 0), body: form.get('body') })
    setCommenting(null)
    await onRefresh()
  }

  async function prepare() {
    const result = await post<Candidate>('/api/integrations/prepare', { taskId: detail.task.id, checkpointId: checkpoint.id })
    setCandidate(result)
    if (result.state === 'ready') setFinalDiff(await api(`/api/integrations/${result.id}/diff`))
    await onRefresh()
  }

  async function apply() {
    if (!candidate?.commitOid) return
    const result = await post<Integration>(`/api/integrations/${candidate.id}/apply`, { confirmOid: candidate.commitOid })
    setIntegration(result)
    setCandidate({ ...candidate, state: 'applied' })
    await onRefresh()
  }

  async function undo() {
    if (!integration) return
    await post(`/api/undo/${integration.id}`)
    setCandidate(undefined)
    setIntegration(undefined)
    setFinalDiff(undefined)
    await onRefresh()
  }

  async function uploadEvidence(file?: File) {
    if (!file) return
    const body = new FormData()
    body.append('file', file)
    await post(`/api/checkpoints/${checkpoint.id}/artifacts`, body)
    await onRefresh()
  }

  async function choose() {
    await post(`/api/select/${checkpoint.id}`)
    await onRefresh()
  }

  return (
    <section className="review-panel">
      <div className="review-summary">
        <div><p className="eyebrow">Reviewing {path?.name || 'path'}</p><h2>{checkpoint.name}</h2><p>{checkpoint.summary || 'No summary was provided for this checkpoint.'}</p></div>
        <div className="review-actions">{checkpoint.ready && detail.task.selectedCheckpointId !== checkpoint.id && <Button onClick={choose}>Choose this result</Button>}<Button kind="quiet" onClick={() => setCommenting('')}>Leave general feedback</Button></div>
      </div>
      <div className="handoff-grid"><div><span>Verification</span><strong className={checkpoint.verificationStatus === 'passed' ? 'success-text' : ''}>{checkpoint.verificationStatus.replace('-', ' ')}</strong><p>{checkpoint.verificationNotes}</p></div><div><span>Risks & follow-ups</span><p>{checkpoint.riskNotes || 'None noted'}</p></div><div><span>Frozen version</span><strong>{shortOid(checkpoint.commitOid)}</strong><p>{new Date(checkpoint.createdAt).toLocaleString()}</p></div></div>
      <div className="evidence"><div><span>Evidence</span>{evidence.length ? evidence.map((artifact) => <strong key={artifact.id}>{artifact.name} <small>{Math.ceil(artifact.size / 1024)} KB</small></strong>) : <p>No screenshots or notes attached.</p>}</div><label className="button quiet">Attach evidence<input type="file" accept=".png,.jpg,.jpeg,.webp,.md,.txt,.json,.log" onChange={(event) => void uploadEvidence(event.target.files?.[0])} /></label></div>
      <div className="diff-toolbar"><div><h3>Files changed</h3><span>{diff?.files.length || 0} files · <b className="additions">+{diff?.files.reduce((sum, file) => sum + file.additions, 0) || 0}</b> <b className="deletions">−{diff?.files.reduce((sum, file) => sum + file.deletions, 0) || 0}</b></span></div><div className="segmented"><button className={view === 'unified' ? 'active' : ''} onClick={() => setView('unified')}>Unified</button><button className={view === 'split' ? 'active' : ''} onClick={() => setView('split')}>Side by side</button></div></div>
      {loading ? <div className="loading">Loading this checkpoint…</div> : <DiffViewer result={diff} view={view} onComment={(file) => setCommenting(file)} />}
      <section className="comments"><div className="section-heading"><h3>Feedback</h3><span>{comments.filter((comment) => comment.status === 'open').length} open</span></div>{comments.length === 0 ? <p className="muted">No feedback yet.</p> : comments.map((comment) => <article className={`comment ${comment.status}`} key={comment.id}><div><strong>{comment.filePath || 'General feedback'}{comment.startLine ? ` · line ${comment.startLine}${comment.endLine && comment.endLine !== comment.startLine ? `–${comment.endLine}` : ''}` : ''}</strong><p>{comment.body}</p></div><Button kind="quiet" onClick={async () => { await post(`/api/comments/${comment.id}/${comment.status === 'open' ? 'resolve' : 'reopen'}`); await onRefresh() }}>{comment.status === 'open' ? 'Resolve' : 'Reopen'}</Button></article>)}</section>
      {checkpoint.ready && <section className="integration"><div><p className="eyebrow">Finish</p><h3>Apply this result to {detail.task.baseRef.replace('refs/heads/', '')}</h3><p>Ooh prepares one clean commit in isolation. You’ll see the exact final result before your main branch changes.</p></div>{!candidate && <Button kind="primary" onClick={prepare}>Prepare final result</Button>}{candidate?.state === 'needs_resolution' && <div className="notice warning"><strong>A conflict needs help.</strong><p>Resolve it in <code>{candidate.worktreePath}</code>, then run <code>ooh integrate resolve {candidate.id}</code>.</p></div>}{candidate?.state === 'ready' && <div className="final-preview"><h4>Final result · {shortOid(candidate.commitOid)}</h4><DiffViewer result={finalDiff} view={view} /><div className="confirm-bar"><span>This archives the task paths and moves the base branch forward.</span><Button kind="primary" onClick={apply}>Apply to {detail.task.baseRef.replace('refs/heads/', '')}</Button></div></div>}{candidate?.state === 'applied' && <div className="notice success"><strong>Applied successfully.</strong><p>Undo is available while this remains the clean base tip.</p>{integration && <Button onClick={undo}>Undo integration</Button>}</div>}</section>}
      {commenting !== null && <Modal title="Leave feedback" onClose={() => setCommenting(null)}><form className="form" onSubmit={addComment}><label>File<input name="filePath" defaultValue={commenting} placeholder="General feedback" /></label><div className="form-row"><label>Side<select name="side" defaultValue="new"><option value="new">New version</option><option value="old">Old version</option></select></label><label>Line or range<input name="startLine" type="number" min="1" placeholder="Start line" /></label><label>End line<input name="endLine" type="number" min="1" placeholder="Optional" /></label></div><label>Feedback<textarea name="body" autoFocus required placeholder="Explain what should change and why" /></label><div className="modal-actions"><Button kind="primary">Add feedback</Button></div></form></Modal>}
    </section>
  )
}

function DiffViewer({ result, view, onComment }: { result?: DiffResult; view: 'unified' | 'split'; onComment?: (file: string) => void }) {
  const parsed = useMemo(() => {
    if (!result?.patch) return []
    try { return parseDiff(result.patch) } catch { return [] }
  }, [result?.patch])
  if (!result?.files.length) return <div className="empty-diff">No file changes in this comparison.</div>
  if (!parsed.length) return <pre className="raw-diff">{result.patch}</pre>
  return <div className="diff-stack">{parsed.map((file, index) => { const path = file.newPath || file.oldPath || `File ${index + 1}`; return <article className="diff-file" key={`${path}-${index}`}><header><code>{path.replace(/^[ab]\//, '')}</code>{onComment && <Button kind="quiet" onClick={() => onComment(path.replace(/^[ab]\//, ''))}>Comment</Button>}</header><div className="diff-scroll"><Diff viewType={view} diffType={file.type} hunks={file.hunks}>{(hunks) => hunks.map((hunk) => <Hunk key={hunk.content} hunk={hunk} />)}</Diff></div></article> })}</div>
}

function TaskPage({ detail, selectedCheckpoint, onBack, onRefresh, onReview }: { detail: TaskDetail; selectedCheckpoint?: string; onBack: () => void; onRefresh: () => Promise<void>; onReview: (id: string) => void }) {
  const checkpoint = detail.checkpoints.find((item) => item.id === selectedCheckpoint)
  const sorted = [...detail.checkpoints].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
  return <main className="page task-page"><button className="back" onClick={onBack}>← All tasks</button><div className="task-header"><div><div className="title-status"><span className={`status-dot ${detail.task.state}`} /><span>{stateLabel[detail.task.state] || detail.task.state}</span></div><h1>{detail.task.title}</h1><p>{detail.task.description || 'No description provided.'}</p></div><div className="task-facts"><span>Base branch<strong>{detail.task.baseRef.replace('refs/heads/', '')}</strong></span><span>Task ID<strong>{shortId(detail.task.id)}</strong></span></div></div><section><div className="section-heading"><div><p className="eyebrow">Parallel work</p><h2>Paths</h2></div><span>{detail.paths.filter((path) => path.state !== 'deleted').length}</span></div><div className="paths-grid">{detail.paths.filter((path) => path.state !== 'deleted').map((path) => <PathCard key={path.id} path={path} detail={detail} onRefresh={onRefresh} onReview={onReview} />)}</div></section>{sorted.length > 0 && <div className="review-layout"><aside className="timeline"><p className="eyebrow">Saved versions</p>{sorted.map((item) => <button className={item.id === checkpoint?.id ? 'active' : ''} key={item.id} onClick={() => onReview(item.id)}><span className={item.ready ? 'ready-marker' : ''} /><div><strong>{item.name}</strong><small>{detail.paths.find((path) => path.id === item.pathId)?.name} · {new Date(item.createdAt).toLocaleDateString()}</small></div></button>)}</aside>{checkpoint ? <ReviewPanel checkpoint={checkpoint} detail={detail} onRefresh={onRefresh} /> : <div className="review-placeholder">Choose a saved version to review.</div>}</div>}</main>
}

export default function App() {
  const [bootstrap, setBootstrap] = useState<Bootstrap>()
  const [tasks, setTasks] = useState<Task[]>([])
  const [taskId, setTaskId] = useState<string>()
  const [detail, setDetail] = useState<TaskDetail>()
  const [checkpointId, setCheckpointId] = useState<string>()
  const [agents, setAgents] = useState<AgentInfo[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [setupOpen, setSetupOpen] = useState(false)
  const [setupDraftId, setSetupDraftId] = useState<string>()
  const [startingTask, setStartingTask] = useState(false)

  const loadTasks = useCallback(async () => setTasks((await api<Task[]>('/api/tasks')) || []), [])
  const loadDetail = useCallback(async () => { if (!taskId) return; const value = await api<TaskDetail>(`/api/tasks/${taskId}`); setDetail(value); setCheckpointId((current) => current || value.task.selectedCheckpointId || [...value.checkpoints].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))[0]?.id) }, [taskId])

  useEffect(() => { Promise.all([api<Bootstrap>('/api/bootstrap'), api<Task[]>('/api/tasks'), api<AgentInfo[]>('/api/agents')]).then(([info, items, detected]) => { setBootstrap(info); setTasks(items || []); setAgents(detected || []) }).catch((reason) => setError(reason.message)).finally(() => setLoading(false)) }, [])
  useEffect(() => { const handler = (event: PromiseRejectionEvent) => { event.preventDefault(); setError(event.reason instanceof Error ? event.reason.message : String(event.reason)) }; window.addEventListener('unhandledrejection', handler); return () => window.removeEventListener('unhandledrejection', handler) }, [])
  useEffect(() => { if (!taskId) { setDetail(undefined); setCheckpointId(undefined); return } loadDetail().catch((reason) => setError(reason.message)) }, [taskId, loadDetail])
  useEffect(() => { const timer = window.setInterval(() => { loadTasks().catch(() => undefined); if (taskId) loadDetail().catch(() => undefined) }, 4000); return () => window.clearInterval(timer) }, [loadTasks, loadDetail, taskId])

  async function refresh() { setError(''); try { await Promise.all([loadTasks(), loadDetail()]) } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) } }
  function configureNewTask() { setSetupDraftId(undefined); setSetupOpen(true) }
  function openTask(id: string) {
    const task = tasks.find((item) => item.id === id)
    if (task?.state === 'draft') { setSetupDraftId(id); setSetupOpen(true); return }
    setTaskId(id)
  }
  async function startTask(input: TaskSetup) {
    setStartingTask(true)
    let draftId = setupDraftId
    let createdHere = false
    try {
      if (!draftId) {
        const draft = await post<TaskDetail>('/api/tasks/draft')
        draftId = draft.task.id
        createdHere = true
      }
      const value = await post<TaskDetail>(`/api/tasks/${draftId}/start`, input)
      setSetupOpen(false)
      setSetupDraftId(undefined)
      setTaskId(value.task.id)
      setDetail(value)
      await loadTasks()
    } catch (reason) {
      if (createdHere && draftId) {
        try { await api(`/api/tasks/${draftId}/draft`, { method: 'DELETE' }) } catch { /* Keep a changed draft if startup reached the agent. */ }
      }
      setError(reason instanceof Error ? reason.message : String(reason))
    } finally { setStartingTask(false) }
  }
  async function leaveTask() { if (detail?.task.state === 'draft') { try { await api(`/api/tasks/${detail.task.id}/draft`, { method: 'DELETE' }) } catch { /* A changed draft is intentionally kept. */ } } setTaskId(undefined); await loadTasks() }

  if (loading) return <div className="splash"><div className="brand-mark">ooh</div><p>Opening your workspace…</p></div>
  return <><header className="app-header"><button className="brand" onClick={() => void leaveTask()}><span className="brand-mark">ooh</span><span><strong>{bootstrap?.repoName || 'Repository'}</strong><small>Parallel work, clearly reviewed</small></span></button><div className="header-meta"><span>Local only</span><span>v{bootstrap?.version || 'dev'}</span></div></header>{error && <div className="error-banner" role="alert"><span>{error}</span><button onClick={() => setError('')}>Dismiss</button></div>}{taskId && detail ? <Workspace detail={detail} agents={agents} onBack={leaveTask} onRefresh={refresh} renderReview={(checkpoint) => <ReviewPanel checkpoint={checkpoint} detail={detail} onRefresh={refresh} />} /> : <Overview tasks={tasks} onOpen={openTask} onCreate={configureNewTask} />}{setupOpen && <TaskSetupModal agents={agents} starting={startingTask} onClose={() => { if (!startingTask) { setSetupOpen(false); setSetupDraftId(undefined) } }} onStart={startTask} />}</>
}
