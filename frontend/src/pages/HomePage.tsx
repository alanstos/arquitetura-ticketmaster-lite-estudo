import { FormEvent, useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { EventSummary, formatDate, formatPrice, listEvents, searchEvents } from '../api'

// Os filtros ficam na query string (/?keyword=rio&startDate=...), então a busca é compartilhável.
export default function HomePage() {
  const [params, setParams] = useSearchParams()
  const keyword = params.get('keyword') ?? ''
  const startDate = params.get('startDate') ?? ''
  const endDate = params.get('endDate') ?? ''

  const [events, setEvents] = useState<EventSummary[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const hasFilter = keyword || startDate || endDate
    setError(null)
    ;(hasFilter ? searchEvents({ keyword, startDate, endDate }) : listEvents())
      .then(setEvents)
      .catch((e: Error) => setError(e.message))
  }, [keyword, startDate, endDate])

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const fd = new FormData(e.currentTarget)
    const next = new URLSearchParams()
    for (const k of ['keyword', 'startDate', 'endDate']) {
      const v = String(fd.get(k) ?? '').trim()
      if (v) next.set(k, v)
    }
    setParams(next)
  }

  return (
    <>
      <form className="search" onSubmit={onSubmit} key={params.toString()}>
        <label>
          Buscar
          <input name="keyword" placeholder="Evento, local ou cidade" defaultValue={keyword} />
        </label>
        <label>
          De
          <input type="date" name="startDate" defaultValue={startDate} />
        </label>
        <label>
          Até
          <input type="date" name="endDate" defaultValue={endDate} />
        </label>
        <button type="submit">Pesquisar</button>
        {(keyword || startDate || endDate) && (
          <button type="button" className="secondary" onClick={() => setParams({})}>
            Limpar
          </button>
        )}
      </form>

      {error && <p className="alert error">{error}</p>}
      {!events && !error && <p>Carregando…</p>}
      {events?.length === 0 && <p>Nenhum evento encontrado.</p>}

      <ul className="event-list">
        {events?.map((ev) => (
          <li key={ev.id} className="card">
            <Link to={`/event/${ev.slug}`}>
              <h2>{ev.name}</h2>
            </Link>
            <p className="muted">{formatDate(ev.starts_at)}</p>
            <p>
              {ev.venue.name} — {ev.venue.city}
            </p>
            <p className="card-footer">
              <span>{formatPrice(ev.price_cents)}</span>
              <span className={ev.available_seats === 0 ? 'badge sold' : 'badge'}>
                {ev.available_seats === 0
                  ? 'Esgotado'
                  : `${ev.available_seats}/${ev.total_seats} lugares livres`}
              </span>
            </p>
          </li>
        ))}
      </ul>
    </>
  )
}
