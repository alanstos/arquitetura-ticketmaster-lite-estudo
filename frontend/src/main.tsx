import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter, Link, Route, Routes } from 'react-router-dom'
import HomePage from './pages/HomePage'
import EventPage from './pages/EventPage'
import AdminPage from './pages/AdminPage'
import './styles.css'

function App() {
  return (
    <BrowserRouter>
      <header className="topbar">
        <Link to="/" className="brand">🎟️ Ticketmaster Lite</Link>
        <Link to="/admin" className="topbar-link">Relatório</Link>
      </header>
      <main className="container">
        <Routes>
          <Route path="/" element={<HomePage />} />
          <Route path="/event/:slug" element={<EventPage />} />
          <Route path="/admin" element={<AdminPage />} />
          <Route path="*" element={<p>Página não encontrada. <Link to="/">Voltar</Link></p>} />
        </Routes>
      </main>
    </BrowserRouter>
  )
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
