import { FormEvent, useEffect, useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { AdminTicket, SeatStatus, Venue, adminTickets, formatPrice, listVenues } from '../api'

// Relatório administrativo: todos os assentos (vendidos, reservados e disponíveis) com o comprador.
// Filtros de servidor (local + datas) e de status ficam na query string, como na HomePage.
// Sem polling: os dados só são recarregados ao mudar os filtros ou clicar em "Atualizar".

const STATUS_LABEL: Record<SeatStatus, string> = {
  SOLD: 'Vendido',
  RESERVED: 'Reservado',
  AVAILABLE: 'Disponível',
}
const STATUS_ORDER: SeatStatus[] = ['SOLD', 'RESERVED', 'AVAILABLE']

const shortDate = (iso: string) =>
  new Date(iso).toLocaleString('pt-BR', {
    timeZone: 'America/Sao_Paulo',
    dateStyle: 'short',
    timeStyle: 'short',
  })

export default function AdminPage() {
  const [params, setParams] = useSearchParams()
  const venueId = params.get('venueId') ?? ''
  const startDate = params.get('startDate') ?? ''
  const endDate = params.get('endDate') ?? ''
  const status = (params.get('status') ?? '') as SeatStatus | ''

  const [venues, setVenues] = useState<Venue[]>([])
  const [tickets, setTickets] = useState<AdminTicket[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  // Incrementado pelo botão "Atualizar" para disparar o efeito de novo com os mesmos filtros.
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    listVenues().then(setVenues).catch(() => setVenues([]))
  }, [])

  useEffect(() => {
    // `ignore` descarta respostas antigas se o filtro mudar antes da anterior voltar.
    let ignore = false
    setLoading(true)
    setError(null)
    adminTickets({ venueId, startDate, endDate })
      .then((data) => {
        if (ignore) return
        setTickets(data)
        setUpdatedAt(new Date())
      })
      .catch((e: Error) => !ignore && setError(e.message))
      .finally(() => !ignore && setLoading(false))
    return () => {
      ignore = true
    }
  }, [venueId, startDate, endDate, reloadKey])

  const counts = useMemo(() => {
    const c: Record<SeatStatus, number> = { SOLD: 0, RESERVED: 0, AVAILABLE: 0 }
    let revenue = 0
    for (const t of tickets ?? []) {
      c[t.status]++
      if (t.status === 'SOLD') revenue += t.price_cents
    }
    return { ...c, revenue }
  }, [tickets])

  const visible = useMemo(
    () => (tickets ?? []).filter((t) => !status || t.status === status),
    [tickets, status],
  )

  function setParam(key: string, value: string) {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }

  function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const fd = new FormData(e.currentTarget)
    const next = new URLSearchParams()
    for (const k of ['venueId', 'startDate', 'endDate']) {
      const v = String(fd.get(k) ?? '').trim()
      if (v) next.set(k, v)
    }
    if (status) next.set('status', status)
    setParams(next)
  }

  const hasFilter = venueId || startDate || endDate || status

  return (
    <section className="admin">
      <div className="admin-header">
        <div>
          <h1>Relatório de ingressos</h1>
          <p className="muted">
            {updatedAt ? `Atualizado às ${updatedAt.toLocaleTimeString('pt-BR')}` : 'Carregando…'}
          </p>
        </div>
        <button
          type="button"
          className="refresh"
          onClick={() => setReloadKey((k) => k + 1)}
          disabled={loading}
          aria-label="Atualizar relatório"
        >
          <RefreshIcon spinning={loading} />
          Atualizar
        </button>
      </div>

      <form className="search" onSubmit={onSubmit} key={`${venueId}|${startDate}|${endDate}`}>
        <label>
          Local
          <select name="venueId" defaultValue={venueId}>
            <option value="">Todos os locais</option>
            {venues.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name} — {v.city}
              </option>
            ))}
          </select>
        </label>
        <label>
          Evento de
          <input type="date" name="startDate" defaultValue={startDate} />
        </label>
        <label>
          Até
          <input type="date" name="endDate" defaultValue={endDate} />
        </label>
        <button type="submit">Filtrar</button>
        {hasFilter && (
          <button type="button" className="secondary" onClick={() => setParams({})}>
            Limpar
          </button>
        )}
      </form>

      <div className="status-chips" role="group" aria-label="Filtrar por status">
        <button
          type="button"
          className={!status ? 'chip active' : 'chip'}
          onClick={() => setParam('status', '')}
        >
          Todos <b>{tickets?.length ?? 0}</b>
        </button>
        {STATUS_ORDER.map((s) => (
          <button
            key={s}
            type="button"
            className={status === s ? `chip ${s.toLowerCase()} active` : `chip ${s.toLowerCase()}`}
            onClick={() => setParam('status', s)}
          >
            {STATUS_LABEL[s]} <b>{counts[s]}</b>
          </button>
        ))}
        <span className="revenue">
          Receita (vendidos): <b>{formatPrice(counts.revenue)}</b>
        </span>
      </div>

      {error && <p className="alert error">{error}</p>}
      {tickets && visible.length === 0 && <p>Nenhum ingresso encontrado para os filtros.</p>}

      {visible.length > 0 && (
        <div className="table-wrap">
          <table className="report">
            <thead>
              <tr>
                <th>Evento</th>
                <th>Data</th>
                <th>Local</th>
                <th>Assento</th>
                <th>Status</th>
                <th>Comprador</th>
                <th>E-mail</th>
                <th>Compra / prazo</th>
                <th className="num">Valor</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((t) => (
                <tr key={t.ticket_id}>
                  <td>
                    <Link to={`/event/${t.event_slug}`}>{t.event_name}</Link>
                  </td>
                  <td className="nowrap">{shortDate(t.event_starts_at)}</td>
                  <td>
                    {t.venue_name}
                    <div className="muted small">{t.venue_city}</div>
                  </td>
                  <td>
                    <b>{t.seat}</b>
                  </td>
                  <td>
                    <span className={`status-pill ${t.status.toLowerCase()}`}>
                      {STATUS_LABEL[t.status]}
                    </span>
                  </td>
                  <td>{t.buyer?.name ?? <span className="muted">—</span>}</td>
                  <td>{t.buyer?.email ?? <span className="muted">—</span>}</td>
                  <td className="nowrap">
                    {t.buyer?.confirmed_at
                      ? shortDate(t.buyer.confirmed_at)
                      : t.buyer?.reserved_until
                        ? `hold até ${shortDate(t.buyer.reserved_until)}`
                        : <span className="muted">—</span>}
                  </td>
                  <td className="num nowrap">{formatPrice(t.price_cents)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

// Ícone de "atualizar" (setas circulares) em SVG inline — sem dependência de biblioteca de ícones.
function RefreshIcon({ spinning }: { spinning: boolean }) {
  return (
    <svg
      className={spinning ? 'icon spin' : 'icon'}
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M21 12a9 9 0 1 1-2.64-6.36" />
      <polyline points="21 3 21 9 15 9" />
    </svg>
  )
}
