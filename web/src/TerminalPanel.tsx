import { useEffect, useRef, useState } from 'react'
import '@xterm/xterm/css/xterm.css'
import { post } from './api'

export default function TerminalPanel({ pathId }: { pathId: string }) {
  const host = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState('Connecting…')

  useEffect(() => {
    if (!host.current) return
    let socket: WebSocket | undefined
    let disposed = false
    let terminal: import('@xterm/xterm').Terminal | undefined
    let resize: ResizeObserver | undefined
    void Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit')]).then(async ([xterm, addon]) => {
      if (disposed || !host.current) return
      terminal = new xterm.Terminal({ cursorBlink: true, convertEol: true, fontSize: 12, fontFamily: 'SFMono-Regular, Consolas, monospace', theme: { background: '#171a17', foreground: '#e7eae4', cursor: '#7dc39a' } })
      const fit = new addon.FitAddon()
      terminal.loadAddon(fit)
      terminal.open(host.current)
      fit.fit()
      const { id, ticket } = await post<{ id: string; ticket: string }>(`/api/paths/${pathId}/terminals`)
      if (disposed || !terminal) return
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      socket = new WebSocket(`${protocol}//${window.location.host}/ws/terminals/${id}?ticket=${encodeURIComponent(ticket)}`)
      socket.binaryType = 'arraybuffer'
      socket.onopen = () => { setStatus(''); socket?.send(JSON.stringify({ type: 'resize', cols: terminal?.cols, rows: terminal?.rows })) }
      socket.onmessage = (event) => terminal?.write(event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : String(event.data))
      socket.onclose = () => setStatus('Terminal closed')
      socket.onerror = () => setStatus('Terminal connection failed')
      terminal.onData((data) => socket?.readyState === WebSocket.OPEN && socket.send(JSON.stringify({ type: 'input', data })))
      resize = new ResizeObserver(() => {
        fit.fit()
        if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'resize', cols: terminal?.cols, rows: terminal?.rows }))
      })
      resize.observe(host.current)
    }).catch((error) => setStatus(error instanceof Error ? error.message : String(error)))
    return () => {
      disposed = true
      resize?.disconnect()
      socket?.close()
      terminal?.dispose()
    }
  }, [pathId])

  return <div className="terminal-shell">{status && <span className="terminal-status">{status}</span>}<div className="terminal-host" ref={host} /></div>
}
