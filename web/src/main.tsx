import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import 'react-diff-view/style/index.css'
import './styles.css'
import './workspace.css'
import './evidence.css'
import App from './App'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
