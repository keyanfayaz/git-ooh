import { FormEvent, ReactNode, useEffect, useMemo, useState } from 'react'
import { ChevronDown, ChevronRight, CircleStop, File, FilePlus2, Folder, FolderPlus, GitBranch, Play, Plus, RefreshCw, Search, TerminalSquare, Trash2, X } from 'lucide-react'
import CodeEditor from './CodeEditor'
import TerminalPanel from './TerminalPanel'
import { api, apiBlob, authToken, patch, post, put, remove } from './api'
import type { AgentInfo, AgentRun, Checkpoint, Path, PreviewCapability, PreviewSession, SearchMatch, TaskDetail, TreeEntry, WorkspaceFile } from './types'

type Tab = { id: string; pathId: string; kind: 'file' | 'image' | 'preview' | 'review'; label: string; path?: string; checkpointId?: string }
type Buffer = { file: WorkspaceFile; value: string; dirty: boolean; external: boolean }
type Variation = { name: string; provider: string; model: string; guidance: string }

const imagePattern = /\.(png|jpe?g|gif|webp|svg|ico)$/i

function IconButton({ label, children, ...props }: React.ButtonHTMLAttributes<HTMLButtonElement> & { label: string; children: ReactNode }) {
  return <button className="workspace-icon-button" aria-label={label} title={label} {...props}>{children}</button>
}

function ExplorerNode({ pathId, entry, onOpen, onChanged }: { pathId: string; entry: TreeEntry; onOpen: (entry: TreeEntry) => void; onChanged: () => void }) {
  const [expanded, setExpanded] = useState(false)
  const [children, setChildren] = useState<TreeEntry[]>([])
  const [loading, setLoading] = useState(false)

  async function toggle() {
    if (entry.kind === 'file') { onOpen(entry); return }
    const next = !expanded
    setExpanded(next)
    if (next && children.length === 0) {
      setLoading(true)
      try { setChildren(await api(`/api/paths/${pathId}/tree?path=${encodeURIComponent(entry.path)}`)) } finally { setLoading(false) }
    }
  }

  async function renameEntry(event: React.MouseEvent) {
    event.stopPropagation()
    const next = window.prompt('Rename to', entry.path)
    if (!next || next === entry.path) return
    await patch(`/api/paths/${pathId}/entries`, { path: entry.path, destination: next })
    onChanged()
  }

  async function deleteEntry(event: React.MouseEvent) {
    event.stopPropagation()
    if (!window.confirm(`Delete ${entry.path}? The change remains recoverable in Git until it is committed.`)) return
    await remove(`/api/paths/${pathId}/entries`, { path: entry.path })
    onChanged()
  }

  return <div className="explorer-node">
    <div className="explorer-row" onClick={() => void toggle()}>
      <span className="explorer-chevron">{entry.kind === 'directory' ? (expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />) : null}</span>
      {entry.kind === 'directory' ? <Folder size={15} /> : <File size={15} />}
      <span>{entry.name}</span>
      <span className="explorer-actions"><button onClick={renameEntry}>Rename</button><button onClick={deleteEntry}>Delete</button></span>
    </div>
    {expanded && <div className="explorer-children">{loading ? <span className="explorer-loading">Loading…</span> : children.map((child) => <ExplorerNode key={child.path} pathId={pathId} entry={child} onOpen={onOpen} onChanged={onChanged} />)}</div>}
  </div>
}

function FileExplorer({ pathId, onOpen }: { pathId: string; onOpen: (entry: TreeEntry) => void }) {
  const [entries, setEntries] = useState<TreeEntry[]>([])
  const [version, setVersion] = useState(0)
  const [query, setQuery] = useState('')
  const [matches, setMatches] = useState<SearchMatch[]>([])
  const [searching, setSearching] = useState(false)

  useEffect(() => { void api<TreeEntry[]>(`/api/paths/${pathId}/tree`).then(setEntries) }, [pathId, version])
  useEffect(() => {
    if (query.trim().length < 2) { setMatches([]); return }
    const timer = window.setTimeout(() => {
      setSearching(true)
      void api<SearchMatch[]>(`/api/paths/${pathId}/search?q=${encodeURIComponent(query)}`).then(setMatches).finally(() => setSearching(false))
    }, 250)
    return () => window.clearTimeout(timer)
  }, [pathId, query])

  async function create(kind: 'file' | 'directory') {
    const value = window.prompt(kind === 'file' ? 'New file path' : 'New folder path')
    if (!value) return
    await post(`/api/paths/${pathId}/entries`, { path: value, kind })
    setVersion((current) => current + 1)
    if (kind === 'file') onOpen({ name: value.split('/').pop() || value, path: value, kind: 'file' })
  }

  return <aside className="file-explorer">
    <div className="panel-heading"><strong>Explorer</strong><div><IconButton label="New file" onClick={() => void create('file')}><FilePlus2 size={15} /></IconButton><IconButton label="New folder" onClick={() => void create('directory')}><FolderPlus size={15} /></IconButton><IconButton label="Refresh files" onClick={() => setVersion((current) => current + 1)}><RefreshCw size={14} /></IconButton></div></div>
    <label className="explorer-search"><Search size={14} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search files" /></label>
    <div className="explorer-tree">
      {query.trim().length >= 2 ? (searching ? <p>Searching…</p> : matches.map((match) => <button className="search-result" key={`${match.path}:${match.line}`} onClick={() => onOpen({ name: match.path.split('/').pop() || match.path, path: match.path, kind: 'file' })}><strong>{match.path}</strong><small>Line {match.line} · {match.preview}</small></button>)) : entries.map((entry) => <ExplorerNode key={entry.path} pathId={pathId} entry={entry} onOpen={onOpen} onChanged={() => setVersion((current) => current + 1)} />)}
    </div>
  </aside>
}

function ImagePreview({ pathId, path }: { pathId: string; path: string }) {
  const [source, setSource] = useState('')
  useEffect(() => {
    let objectURL = ''
    void apiBlob(`/api/paths/${pathId}/raw?path=${encodeURIComponent(path)}`).then((blob) => { objectURL = URL.createObjectURL(blob); setSource(objectURL) })
    return () => { if (objectURL) URL.revokeObjectURL(objectURL) }
  }, [pathId, path])
  return <div className="image-preview">{source ? <img src={source} alt={path} /> : <span>Loading image…</span>}</div>
}

function AgentLog({ run }: { run: AgentRun }) {
  const [content, setContent] = useState('')
  useEffect(() => {
    let active = true
    const load = () => void api<{ content: string }>(`/api/runs/${run.id}/log`).then((value) => active && setContent(value.content)).catch(() => undefined)
    load()
    const timer = window.setInterval(load, run.state === 'running' || run.state === 'queued' ? 900 : 4000)
    return () => { active = false; window.clearInterval(timer) }
  }, [run.id, run.state])
  return <pre className="agent-log">{content || (run.state === 'queued' ? 'Waiting to start…' : 'Agent output will appear here.')}</pre>
}

function WorkPanel({ detail, path, agents, onRefresh }: { detail: TaskDetail; path: Path; agents: AgentInfo[]; onRefresh: () => Promise<void> }) {
  const available = agents.filter((agent) => agent.available)
  const remembered = (typeof window.sessionStorage?.getItem === 'function' ? window.sessionStorage.getItem('ooh-agent') : '') || available[0]?.id || ''
  const [followup, setFollowup] = useState('')
  const [addingVariation, setAddingVariation] = useState(false)
  const [newVariation, setNewVariation] = useState<Variation>({ name: 'Another approach', provider: remembered, model: '', guidance: '' })
  const runs = detail.runs.filter((run) => run.pathId === path.id)
  const latest = runs.at(-1)

  async function continueWork(event: FormEvent) {
    event.preventDefault()
    if (!followup.trim()) return
    await post(`/api/paths/${path.id}/continue`, { provider: latest?.provider || remembered, prompt: followup })
    setFollowup('')
    await onRefresh()
  }

  async function addVariation(event: FormEvent) {
    event.preventDefault()
    await post(`/api/paths/${path.id}/variations`, { variations: [newVariation] })
    setAddingVariation(false)
    setNewVariation({ name: 'Another approach', provider: remembered, model: '', guidance: '' })
    await onRefresh()
  }

  async function saveForReview() {
    const summary = window.prompt('Describe the current result for reviewers')
    if (!summary) return
    await post(`/api/paths/${path.id}/ready`, { summary, verificationStatus: 'not-run', verificationNotes: 'Saved manually from the Ooh workspace.' })
    await onRefresh()
  }

  if (detail.task.state === 'draft') return <aside className="work-panel"><div className="work-panel-head"><div><span className="eyebrow">Draft</span><h2>Not started</h2></div></div><div className="work-empty">Return to Tasks to configure this task before opening the workspace.</div></aside>

  return <aside className="work-panel">
    <div className="work-panel-head"><div><span className="eyebrow">Work</span><h2>{path.name}</h2></div>{latest && <span className={`run-state ${latest.state}`}><i />{latest.state === 'running' ? 'Working' : latest.state}</span>}</div>
    {latest ? <>
      <div className="run-summary"><strong>{latest.provider === 'claude' ? 'Claude Code' : 'Codex'}{latest.model ? ` · ${latest.model}` : ''}</strong><span>{new Date(latest.createdAt).toLocaleTimeString()}</span>{latest.summary && <p>{latest.summary}</p>}{latest.error && <p className="run-error">{latest.error}</p>}</div>
      <AgentLog run={latest} />
      {(latest.state === 'running' || latest.state === 'queued') ? <button className="stop-run" onClick={async () => { await post(`/api/runs/${latest.id}/stop`); await onRefresh() }}><CircleStop size={15} /> Stop agent</button> : <form className="followup-form" onSubmit={continueWork}><textarea value={followup} onChange={(event) => setFollowup(event.target.value)} placeholder="Ask for another change or clarify the result" /><button disabled={!followup.trim()}>Continue work</button></form>}
    </> : <div className="work-empty">This path has no agent run yet.</div>}
    {!latest || (latest.state !== 'running' && latest.state !== 'queued') ? <div className="work-secondary-actions"><button onClick={() => setAddingVariation((current) => !current)}><Plus size={14} /> Try another approach</button><button onClick={() => void saveForReview()}>Save current changes for review</button></div> : null}
    {addingVariation && <form className="add-variation-form" onSubmit={addVariation}><input value={newVariation.name} onChange={(event) => setNewVariation((current) => ({ ...current, name: event.target.value }))} placeholder="Approach name" /><select aria-label="Coding agent" value={newVariation.provider} onChange={(event) => setNewVariation((current) => ({ ...current, provider: event.target.value, model: '' }))}>{available.map((agent) => <option key={agent.id} value={agent.id}>{agent.name}</option>)}</select><select aria-label="Model" value={newVariation.model} onChange={(event) => setNewVariation((current) => ({ ...current, model: event.target.value }))}>{(available.find((agent) => agent.id === newVariation.provider)?.models || [{ id: '', name: 'Default' }]).map((model) => <option key={model.id || 'default'} value={model.id}>{model.name}</option>)}</select><input value={newVariation.guidance} onChange={(event) => setNewVariation((current) => ({ ...current, guidance: event.target.value }))} placeholder="Optional direction" /><button>Start variation</button></form>}
  </aside>
}

function PreviewConfirm({ capability, onCancel, onStart }: { capability: PreviewCapability; onCancel: () => void; onStart: (input: { installCommand: string; runCommand: string; expectedUrl: string; approved: boolean }) => void }) {
  const [installCommand, setInstallCommand] = useState(capability.installCommand || '')
  const [runCommand, setRunCommand] = useState(capability.runCommand || '')
  const [expectedUrl, setExpectedUrl] = useState(capability.expectedUrl || '')
  return <div className="preview-confirm"><div><span className="eyebrow">Local preview</span><h2>Confirm commands once</h2><p>Ooh detected a {capability.kind?.toLowerCase()}. These commands run inside this isolated clone using your local account.</p></div>{installCommand !== '' && <label>Prepare dependencies<input value={installCommand} onChange={(event) => setInstallCommand(event.target.value)} /></label>}<label>Run application<input value={runCommand} onChange={(event) => setRunCommand(event.target.value)} /></label><label>Expected address<input value={expectedUrl} onChange={(event) => setExpectedUrl(event.target.value)} /></label><div><button onClick={onCancel}>Cancel</button><button className="primary" onClick={() => onStart({ installCommand, runCommand, expectedUrl, approved: true })}>Confirm and run</button></div></div>
}

export default function Workspace({ detail, agents, onBack, onRefresh, renderReview }: { detail: TaskDetail; agents: AgentInfo[]; onBack: () => Promise<void>; onRefresh: () => Promise<void>; renderReview: (checkpoint: Checkpoint) => ReactNode }) {
  const activePaths = detail.paths.filter((path) => !['archived', 'deleted'].includes(path.state))
  const [pathId, setPathId] = useState(detail.task.selectedPathId || activePaths[0]?.id)
  const path = activePaths.find((item) => item.id === pathId) || activePaths[0]
  const [tabs, setTabs] = useState<Tab[]>([])
  const [activeTab, setActiveTab] = useState('')
  const [buffers, setBuffers] = useState<Record<string, Buffer>>({})
  const [terminalOpen, setTerminalOpen] = useState(false)
  const [capability, setCapability] = useState<PreviewCapability>()
  const [confirmingPreview, setConfirmingPreview] = useState(false)
  const [preview, setPreview] = useState<PreviewSession>()
  const [error, setError] = useState('')
  const active = tabs.find((tab) => tab.id === activeTab)
  const activeBuffer = active?.path ? buffers[active.id] : undefined
  const latestReady = useMemo(() => [...detail.checkpoints].filter((checkpoint) => checkpoint.ready).sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))[0], [detail.checkpoints])
  const lastSucceeded = detail.runs.filter((run) => run.state === 'succeeded').at(-1)

  useEffect(() => {
    if (detail.task.state !== 'draft') return
    const discard = () => { void fetch(`/api/tasks/${detail.task.id}/draft`, { method: 'DELETE', headers: { 'X-Ooh-Token': authToken }, keepalive: true }) }
    window.addEventListener('beforeunload', discard)
    return () => window.removeEventListener('beforeunload', discard)
  }, [detail.task.id, detail.task.state])

  useEffect(() => { if (path) void api<PreviewCapability>(`/api/paths/${path.id}/preview`).then(setCapability).catch(() => setCapability(undefined)) }, [path?.id])
  useEffect(() => {
    if (!lastSucceeded || !latestReady) return
    const idValue = `review:${latestReady.id}`
    setTabs((current) => current.some((tab) => tab.id === idValue) ? current : [...current, { id: idValue, pathId: latestReady.pathId, kind: 'review', label: 'Review changes', checkpointId: latestReady.id }])
    setActiveTab(idValue)
    setPathId(latestReady.pathId)
  }, [lastSucceeded?.id, latestReady?.id])
  useEffect(() => {
    if (!preview || preview.state === 'failed' || preview.state === 'stopped') return
    const timer = window.setInterval(() => void api<PreviewSession>(`/api/previews/${preview.id}`).then(setPreview), 900)
    return () => window.clearInterval(timer)
  }, [preview?.id, preview?.state])
  useEffect(() => {
    if (!active?.path || active.kind !== 'file' || !activeBuffer) return
    let checking = false
    const check = () => {
      if (checking) return
      checking = true
      void api<WorkspaceFile>(`/api/paths/${active.pathId}/file?path=${encodeURIComponent(active.path!)}`).then((file) => {
        setBuffers((current) => {
          const buffer = current[active.id]
          if (!buffer || buffer.file.revision === file.revision) return current
          return { ...current, [active.id]: buffer.dirty ? { ...buffer, external: true } : { file, value: file.content, dirty: false, external: false } }
        })
      }).catch(() => undefined).finally(() => { checking = false })
    }
    const timer = window.setInterval(check, 2500)
    return () => window.clearInterval(timer)
  }, [active?.id, active?.path, active?.pathId, active?.kind, activeBuffer?.file.revision, activeBuffer?.dirty])

  async function openEntry(entry: TreeEntry) {
    if (!path || entry.kind !== 'file') return
    const kind = imagePattern.test(entry.path) ? 'image' : 'file'
    const idValue = `${path.id}:${kind}:${entry.path}`
    setTabs((current) => current.some((tab) => tab.id === idValue) ? current : [...current, { id: idValue, pathId: path.id, kind, path: entry.path, label: entry.name }])
    setActiveTab(idValue)
    if (kind === 'file' && !buffers[idValue]) {
      try {
        const file = await api<WorkspaceFile>(`/api/paths/${path.id}/file?path=${encodeURIComponent(entry.path)}`)
        setBuffers((current) => ({ ...current, [idValue]: { file, value: file.content, dirty: false, external: false } }))
      } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
    }
  }

  async function saveActive() {
    if (!active || !activeBuffer || !active.path) return
    try {
      const file = await put<WorkspaceFile>(`/api/paths/${active.pathId}/file`, { path: active.path, content: activeBuffer.value, revision: activeBuffer.file.revision })
      setBuffers((current) => ({ ...current, [active.id]: { file, value: file.content, dirty: false, external: false } }))
    } catch (reason) {
      setBuffers((current) => ({ ...current, [active.id]: { ...current[active.id], external: true } }))
      setError(reason instanceof Error ? reason.message : String(reason))
    }
  }

  async function reloadActive() {
    if (!active?.path) return
    const file = await api<WorkspaceFile>(`/api/paths/${active.pathId}/file?path=${encodeURIComponent(active.path)}`)
    setBuffers((current) => ({ ...current, [active.id]: { file, value: file.content, dirty: false, external: false } }))
  }

  function closeTab(tab: Tab) {
    const buffer = buffers[tab.id]
    if (buffer?.dirty && !window.confirm(`Close ${tab.label} without saving your browser edits?`)) return
    setTabs((current) => current.filter((item) => item.id !== tab.id))
    if (activeTab === tab.id) setActiveTab(tabs.filter((item) => item.id !== tab.id).at(-1)?.id || '')
  }

  async function openPreview() {
    if (!path || !capability?.available) return
    if (capability.requiresApproval) { setConfirmingPreview(true); return }
    await startPreview({ installCommand: capability.installCommand || '', runCommand: capability.runCommand || '', expectedUrl: capability.expectedUrl || '', approved: true })
  }

  async function renameTask() {
    const title = window.prompt('Task name', detail.task.title)
    if (!title || title.trim() === detail.task.title) return
    await patch(`/api/tasks/${detail.task.id}`, { title: title.trim() })
    await onRefresh()
  }

  async function startPreview(input: { installCommand: string; runCommand: string; expectedUrl: string; approved: boolean }) {
    if (!path) return
    setConfirmingPreview(false)
    const session = await post<PreviewSession>(`/api/paths/${path.id}/preview`, input)
    setPreview(session)
    const idValue = `${path.id}:preview`
    setTabs((current) => current.some((tab) => tab.id === idValue) ? current : [...current, { id: idValue, pathId: path.id, kind: 'preview', label: 'Preview' }])
    setActiveTab(idValue)
  }

  if (!path) return <div className="workspace-empty">This task has no active workspace.</div>
  return <main className="workspace-app">
    <div className="workspace-toolbar">
      <button className="workspace-back" onClick={() => void onBack()}>← Tasks</button>
      <button className="workspace-title" title="Rename task" onClick={() => void renameTask()}><strong>{detail.task.title}</strong><span>Based on {detail.task.baseRef.replace('refs/heads/', '')} · isolated clone</span></button>
      <div className="path-switcher">{activePaths.map((item) => { const running = detail.runs.some((run) => run.pathId === item.id && (run.state === 'running' || run.state === 'queued')); return <button className={item.id === path.id ? 'active' : ''} key={item.id} onClick={() => setPathId(item.id)}><GitBranch size={13} />{item.name}{running && <i className="pulse-dot" />}</button> })}</div>
      <div className="workspace-tools">{capability?.available && <button onClick={() => void openPreview()}><Play size={14} /> Preview</button>}<button className={terminalOpen ? 'active' : ''} onClick={() => setTerminalOpen((current) => !current)}><TerminalSquare size={14} /> Terminal</button></div>
    </div>
    {error && <div className="workspace-error"><span>{error}</span><button onClick={() => setError('')}>Dismiss</button></div>}
    <div className={`workspace-grid ${terminalOpen ? 'terminal-open' : ''}`}>
      <FileExplorer key={`${path.id}:${detail.statuses[path.id]?.changed || 0}:${detail.statuses[path.id]?.headOid || ''}`} pathId={path.id} onOpen={(entry) => void openEntry(entry)} />
      <section className="workspace-center">
        <div className="editor-tabs">{tabs.map((tab) => <button className={tab.id === activeTab ? 'active' : ''} key={tab.id} onClick={() => { setActiveTab(tab.id); setPathId(tab.pathId) }}><span>{tab.label}</span>{buffers[tab.id]?.dirty && <i />}<X size={13} onClick={(event) => { event.stopPropagation(); closeTab(tab) }} /></button>)}</div>
        <div className="editor-surface">
          {!active && <div className="editor-welcome"><div className="brand-mark">ooh</div><h1>{detail.task.state === 'draft' ? 'Describe the change to begin' : 'Open a file to explore this approach'}</h1><p>Files live in an isolated clone. Agent changes appear here as they happen.</p></div>}
          {active?.kind === 'file' && (activeBuffer ? <div className="file-editor-wrap">{activeBuffer.external && <div className="external-change">This file changed outside the editor.<button onClick={() => void reloadActive()}>Reload from disk</button></div>}<div className="file-editor-meta"><span>{active.path}</span><span>{activeBuffer.dirty ? 'Unsaved' : 'Saved'} · Cmd/Ctrl+S</span></div><CodeEditor key={active.id} value={activeBuffer.value} language={activeBuffer.file.language} onChange={(value) => setBuffers((current) => ({ ...current, [active.id]: { ...current[active.id], value, dirty: value !== current[active.id].file.content } }))} onSave={() => void saveActive()} /></div> : <div className="editor-loading">Opening file…</div>)}
          {active?.kind === 'image' && active.path && <ImagePreview pathId={active.pathId} path={active.path} />}
          {active?.kind === 'preview' && <div className="preview-surface">{preview?.state === 'failed' ? <div className="preview-failed"><h2>Preview did not start</h2><p>{preview.error}</p><pre>{preview.log}</pre></div> : <><div className="preview-bar"><span className={`preview-state ${preview?.state}`} />{preview?.state || 'starting'}<code>{preview?.url}</code>{preview && <button onClick={async () => { await remove(`/api/previews/${preview.id}`); setPreview({ ...preview, state: 'stopped' }) }}>Stop</button>}</div>{preview?.url && preview.state !== 'stopped' ? <iframe title="Application preview" src={preview.url} /> : <div className="preview-waiting">Starting the application…<pre>{preview?.log}</pre></div>}</>}</div>}
          {active?.kind === 'review' && active.checkpointId && <div className="workspace-review">{renderReview(detail.checkpoints.find((checkpoint) => checkpoint.id === active.checkpointId)!)}</div>}
        </div>
      </section>
      <WorkPanel detail={detail} path={path} agents={agents} onRefresh={onRefresh} />
      {terminalOpen && <section className="terminal-panel"><div className="terminal-panel-head"><strong>Terminal · {path.name}</strong><button onClick={() => setTerminalOpen(false)}><X size={14} /> Close</button></div><TerminalPanel key={path.id} pathId={path.id} /></section>}
    </div>
    {confirmingPreview && capability && <div className="workspace-overlay"><PreviewConfirm capability={capability} onCancel={() => setConfirmingPreview(false)} onStart={(input) => void startPreview(input)} /></div>}
  </main>
}
