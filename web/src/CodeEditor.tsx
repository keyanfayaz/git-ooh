import { useEffect, useRef } from 'react'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { css } from '@codemirror/lang-css'
import { go } from '@codemirror/lang-go'
import { html } from '@codemirror/lang-html'
import { javascript } from '@codemirror/lang-javascript'
import { json } from '@codemirror/lang-json'
import { markdown } from '@codemirror/lang-markdown'
import { python } from '@codemirror/lang-python'
import { rust } from '@codemirror/lang-rust'
import { sql } from '@codemirror/lang-sql'
import { yaml } from '@codemirror/lang-yaml'
import { bracketMatching, defaultHighlightStyle, indentOnInput, syntaxHighlighting } from '@codemirror/language'
import { EditorState } from '@codemirror/state'
import { drawSelection, dropCursor, EditorView, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, keymap, lineNumbers, rectangularSelection } from '@codemirror/view'

type Props = {
  value: string
  language: string
  onChange: (value: string) => void
  onSave: () => void
}

const languageExtension = (language: string) => {
  switch (language) {
    case 'javascript': return javascript({ jsx: true, typescript: false })
    case 'typescript': return javascript({ jsx: true, typescript: true })
    case 'json': return json()
    case 'css': return css()
    case 'html': return html()
    case 'markdown': return markdown()
    case 'python': return python()
    case 'rust': return rust()
    case 'go': return go()
    case 'yaml': return yaml()
    case 'sql': return sql()
    default: return []
  }
}

export default function CodeEditor({ value, language, onChange, onSave }: Props) {
  const host = useRef<HTMLDivElement>(null)
  const editor = useRef<EditorView | undefined>(undefined)
  const changing = useRef(false)
  const onChangeRef = useRef(onChange)
  const onSaveRef = useRef(onSave)
  onChangeRef.current = onChange
  onSaveRef.current = onSave

  useEffect(() => {
    if (!host.current) return
    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(), highlightActiveLineGutter(), highlightSpecialChars(), history(), drawSelection(), dropCursor(),
        indentOnInput(), bracketMatching(), rectangularSelection(), highlightActiveLine(), syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        languageExtension(language),
        keymap.of([
          { key: 'Mod-s', preventDefault: true, run: () => { onSaveRef.current(); return true } },
          indentWithTab,
          ...defaultKeymap,
          ...historyKeymap,
        ]),
        EditorView.updateListener.of((update) => {
          if (update.docChanged && !changing.current) onChangeRef.current(update.state.doc.toString())
        }),
        EditorView.theme({
          '&': { height: '100%', fontSize: '13px', backgroundColor: 'var(--editor-bg)' },
          '.cm-scroller': { overflow: 'auto', fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", monospace' },
          '.cm-gutters': { backgroundColor: 'var(--editor-bg)', color: 'var(--editor-gutter)', borderRight: '1px solid var(--workspace-border)' },
          '.cm-activeLine, .cm-activeLineGutter': { backgroundColor: 'var(--editor-active)' },
          '.cm-content': { padding: '10px 0' },
        }),
      ],
    })
    editor.current = new EditorView({ state, parent: host.current })
    return () => { editor.current?.destroy(); editor.current = undefined }
    // A new file/language remounts this component through its key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [language])

  useEffect(() => {
    const view = editor.current
    if (!view || view.state.doc.toString() === value) return
    changing.current = true
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } })
    changing.current = false
  }, [value])

  return <div className="code-editor" ref={host} />
}
