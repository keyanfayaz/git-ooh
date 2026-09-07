type Envelope<T> = { schemaVersion: string; data?: T; error?: { code: string; message: string } }

function findToken() {
  const hash = new URLSearchParams(window.location.hash.replace(/^#/, ''))
  const incoming = hash.get('token')
  if (incoming) {
    sessionStorage.setItem('ooh-token', incoming)
    history.replaceState(null, '', window.location.pathname + window.location.search)
  }
  return incoming || sessionStorage.getItem('ooh-token') || ''
}

const token = findToken()

export const authToken = token

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('X-Ooh-Token', token)
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  const response = await fetch(path, { ...init, headers })
  const payload = (await response.json()) as Envelope<T>
  if (!response.ok || payload.error) throw new Error(payload.error?.message || `Request failed (${response.status})`)
  return payload.data as T
}

export const post = <T>(path: string, body: unknown = {}) =>
  api<T>(path, { method: 'POST', body: body instanceof FormData ? body : JSON.stringify(body) })

export const put = <T>(path: string, body: unknown) => api<T>(path, { method: 'PUT', body: JSON.stringify(body) })
export const patch = <T>(path: string, body: unknown) => api<T>(path, { method: 'PATCH', body: JSON.stringify(body) })
export const remove = <T>(path: string, body?: unknown) => api<T>(path, { method: 'DELETE', body: body === undefined ? undefined : JSON.stringify(body) })

export async function apiBlob(path: string) {
  const response = await fetch(path, { headers: { 'X-Ooh-Token': token } })
  if (!response.ok) throw new Error(`Unable to open file (${response.status})`)
  return response.blob()
}

export const shortOid = (oid?: string) => oid?.slice(0, 8) || '—'
export const shortId = (id: string) => id.split('_').at(-1)?.slice(0, 7) || id.slice(-7)
