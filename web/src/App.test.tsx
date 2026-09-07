import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

const envelope = (data: unknown) => Promise.resolve({ ok: true, json: () => Promise.resolve({ schemaVersion: '1', data }) })

describe('Ooh dashboard', () => {
  beforeEach(() => {
    sessionStorage.setItem('ooh-token', 'test-token')
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/bootstrap')) return envelope({ repoName: 'friendly-repo', version: 'dev', repoId: 'repo', repoRoot: '/repo', retentionDays: 30 })
      return envelope(null)
    }))
  })

  it('explains an empty workspace in approachable language', async () => {
    render(<App />)
    await waitFor(() => expect(screen.getByText('What’s being explored')).toBeInTheDocument())
    expect(screen.getByText('No tasks yet')).toBeInTheDocument()
    expect(screen.getByText('friendly-repo')).toBeInTheDocument()
  })

  it('configures variations in a modal before opening the workspace', async () => {
    const draft = {
      task: { id: 'task_1', title: 'Untitled task', description: '', baseRef: 'refs/heads/main', baseOid: 'abc', baseTipOid: 'abc', state: 'draft', selectedPathId: 'path_1', createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() },
      paths: [{ id: 'path_1', taskId: 'task_1', name: 'Approach 1', branchRef: 'refs/heads/ooh/task/path', worktreePath: '/tmp/path', state: 'working', keep: false, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }],
      forks: [], checkpoints: [], comments: [], artifacts: [], statuses: {}, runs: [],
    }
    const started = {
      ...draft,
      task: { ...draft.task, title: 'Simplify checkout', description: 'Simplify checkout', state: 'working' },
      paths: [draft.paths[0], { ...draft.paths[0], id: 'path_2', name: 'Approach 2', worktreePath: '/tmp/path-2' }],
      runs: [
        { id: 'run_1', taskId: 'task_1', pathId: 'path_1', provider: 'codex', model: 'gpt-test', prompt: 'Simplify checkout', state: 'queued', createdAt: new Date().toISOString() },
        { id: 'run_2', taskId: 'task_1', pathId: 'path_2', provider: 'codex', model: '', prompt: 'Simplify checkout', state: 'queued', createdAt: new Date().toISOString() },
      ],
    }
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/bootstrap')) return envelope({ repoName: 'friendly-repo', version: 'dev', repoId: 'repo', repoRoot: '/repo', retentionDays: 30 })
      if (url.endsWith('/api/agents')) return envelope([{ id: 'codex', name: 'Codex', available: true, models: [{ id: '', name: 'Default' }, { id: 'gpt-test', name: 'gpt-test' }] }])
      if (url.endsWith('/api/tasks/draft') && init?.method === 'POST') return envelope(draft)
      if (url.endsWith('/api/tasks/task_1/start') && init?.method === 'POST') return envelope(started)
      if (url.includes('/api/paths/path_1/tree')) return envelope([])
      if (url.includes('/api/paths/path_1/preview')) return envelope({ available: false, requiresApproval: false })
      if (url.endsWith('/api/tasks')) return envelope([])
      return envelope(draft)
    }))
    render(<App />)
    await waitFor(() => expect(screen.getByText('New task')).toBeInTheDocument())
    fireEvent.click(screen.getByText('New task'))
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'Create a task' })).toBeInTheDocument())
    expect(screen.queryByText('Explorer')).not.toBeInTheDocument()
    fireEvent.change(screen.getByLabelText(/What should change/), { target: { value: 'Simplify checkout' } })
    fireEvent.change(screen.getByLabelText(/Number of variations/), { target: { value: '2' } })
    expect(screen.getByLabelText('Variation 1 coding agent')).toBeInTheDocument()
    expect(screen.getByLabelText('Variation 2 coding agent')).toBeInTheDocument()
    expect(screen.getByLabelText('Variation 1 model')).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Variation 1 model'), { target: { value: 'gpt-test' } })
    fireEvent.click(screen.getByText('Start 2 variations'))
    await waitFor(() => expect(screen.getByText('Explorer')).toBeInTheDocument())
    expect(screen.queryByRole('dialog', { name: 'Create a task' })).not.toBeInTheDocument()
  })
})
