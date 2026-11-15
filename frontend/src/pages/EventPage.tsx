import { FormEvent, useCallback, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  ApiError,
  Booking,
  EventDetail,
  Seat,
  SeatsMessage,
  confirmBooking,
  formatDate,
  formatPrice,
  getEvent,
  reserve,
  seatStreamUrl,
} from '../api'
import SeatMap from '../components/SeatMap'

// Fluxo: selecionar assentos -> POST /reserve (hold com prazo) -> POST /booking (confirma).
// O mapa de assentos é atualizado pelo servidor via SSE (GET /api/events/{slug}/stream).
export default function EventPage() {
  const { slug = '' } = useParams()
  const [event, setEvent] = useState<EventDetail | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [booking, setBooking] = useState<Booking | null>(null)
  // Prazo da reserva no relógio LOCAL (Date.now() + seconds_remaining do servidor).
  // Não usamos expires_at direto para não depender de os relógios estarem sincronizados.
  const [deadline, setDeadline] = useState<number | null>(null)
  const [message, setMessage] = useState<{ kind: 'error' | 'success' | 'info'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  // false enquanto o EventSource está reconectando (mostra um aviso discreto).
  const [live, setLive] = useState(true)

  // Remove da seleção assentos que outra pessoa pegou nesse meio tempo.
  const pruneSelection = useCallback((seats: Seat[]) => {
    setSelected((prev) => {
      const free = new Set(seats.filter((s) => s.status === 'AVAILABLE').map((s) => s.ticket_id))
      const next = new Set([...prev].filter((id) => free.has(id)))
      return next.size === prev.size ? prev : next
    })
  }, [])

  // Dados do evento (nome, local, preço) + mapa inicial: uma vez por slug.
  useEffect(() => {
    let cancelled = false
    getEvent(slug)
      .then((ev) => {
        if (cancelled) return
        setEvent(ev)
        setError(null)
        pruneSelection(ev.seats)
      })
      .catch((e) => {
        if (cancelled) return
        setError(e instanceof ApiError && e.status === 404 ? 'Evento não encontrado.' : (e as Error).message)
      })
    return () => {
      cancelled = true
    }
  }, [slug, pruneSelection])

  // Atualização em tempo real: o servidor manda o mapa completo a cada mudança
  // (reserva, compra ou hold vencido de qualquer usuário). O EventSource reconecta
  // sozinho se a conexão cair, e o servidor manda um snapshot novo ao reconectar.
  useEffect(() => {
    const es = new EventSource(seatStreamUrl(slug))
    es.addEventListener('seats', (e) => {
      const { seats } = JSON.parse((e as MessageEvent<string>).data) as SeatsMessage
      setEvent((ev) => (ev ? { ...ev, seats } : ev))
      pruneSelection(seats)
      setLive(true)
    })
    es.onerror = () => setLive(false)
    return () => es.close()
  }, [slug, pruneSelection])

  const secondsLeft = useCountdown(booking?.status === 'PENDING' ? deadline : null)
  useEffect(() => {
    if (booking?.status === 'PENDING' && secondsLeft !== null && secondsLeft <= 0) {
      // O mapa se atualiza sozinho: o servidor agenda um refresh para o vencimento do hold.
      setBooking(null)
      setMessage({ kind: 'error', text: 'O tempo da sua reserva acabou. Os assentos foram liberados.' })
    }
  }, [secondsLeft, booking])

  if (error) return <p className="alert error">{error} <Link to="/">Voltar</Link></p>
  if (!event) return <p>Carregando…</p>

  const pending = booking?.status === 'PENDING'
  const mine = new Set(pending || booking?.status === 'CONFIRMED' ? booking!.ticket_ids : [])
  const selectedSeats = event.seats.filter((s) => selected.has(s.ticket_id))

  function toggle(id: number) {
    setMessage(null)
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(id) ? next.delete(id) : next.add(id)
      return next
    })
  }

  async function onReserve(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const fd = new FormData(e.currentTarget)
    setBusy(true)
    setMessage(null)
    try {
      const b = await reserve({
        event_id: event!.id,
        ticket_ids: [...selected],
        customer_name: String(fd.get('name')),
        customer_email: String(fd.get('email')),
      })
      setDeadline(Date.now() + b.seconds_remaining * 1000)
      setBooking(b)
      setSelected(new Set())
      setMessage({ kind: 'info', text: 'Assentos reservados! Confirme o pagamento antes do prazo.' })
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setMessage({ kind: 'error', text: 'Ops! Alguém reservou um desses assentos antes de você. Escolha outro.' })
      } else {
        setMessage({ kind: 'error', text: (err as Error).message })
      }
    } finally {
      setBusy(false)
    }
  }

  async function onConfirm() {
    if (!booking) return
    setBusy(true)
    try {
      const b = await confirmBooking(booking.id)
      setBooking(b)
      setMessage({ kind: 'success', text: `Compra confirmada! Assentos: ${b.seats.join(', ')}.` })
    } catch (err) {
      if (err instanceof ApiError && err.status === 410) setBooking(null)
      setMessage({ kind: 'error', text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  return (
    <article className="event-detail">
      <Link to="/">← Todos os eventos</Link>
      <h1>{event.name}</h1>
      <p className="muted">{formatDate(event.starts_at)}</p>
      <p>
        <strong>{event.venue.name}</strong> — {event.venue.address}, {event.venue.city}
      </p>
      {event.description && <p>{event.description}</p>}
      <p>
        Preço por assento: <strong>{formatPrice(event.price_cents)}</strong>
      </p>

      <div className="event-body">
        <SeatMap
          seats={event.seats}
          cols={event.venue.cols}
          selected={selected}
          mine={mine}
          disabled={pending || busy}
          onToggle={toggle}
        />

        <aside className="checkout card">
          {!live && <p className="alert info">Reconectando ao mapa em tempo real…</p>}
          {message &&<p className={`alert ${message.kind}`}>{message.text}</p>}

          {pending && booking && (
            <>
              <h3>Sua reserva</h3>
              <p>Assentos: {booking.seats.join(', ')}</p>
              <p>Total: {formatPrice(booking.total_cents)}</p>
              <p className="countdown">
                Expira em {formatSeconds(secondsLeft ?? 0)}
              </p>
              <button onClick={onConfirm} disabled={busy}>
                Confirmar pagamento
              </button>
            </>
          )}

          {booking?.status === 'CONFIRMED' && (
            <>
              <h3>Ingressos comprados 🎉</h3>
              <p>Pedido: <code>{booking.id}</code></p>
              <button className="secondary" onClick={() => { setBooking(null); setMessage(null) }}>
                Comprar mais
              </button>
            </>
          )}

          {!booking && (
            <form onSubmit={onReserve}>
              <h3>Comprar</h3>
              <p>
                {selectedSeats.length === 0
                  ? 'Selecione um ou mais assentos livres no mapa.'
                  : `Selecionados: ${selectedSeats.map((s) => s.label).join(', ')} — ${formatPrice(
                      selectedSeats.length * event.price_cents,
                    )}`}
              </p>
              <label>
                Nome
                <input name="name" required />
              </label>
              <label>
                E-mail
                <input name="email" type="email" required />
              </label>
              <button type="submit" disabled={busy || selected.size === 0}>
                Reservar assentos
              </button>
            </form>
          )}
        </aside>
      </div>
    </article>
  )
}

// Segundos restantes até `deadline` (ms, relógio local), ou null se não há prazo.
// O valor é calculado durante o render (não guardado em estado): assim nunca existe
// um render com valor "velho" — esse era o bug que expirava a reserva no 1º render.
// O estado `tick` serve só para forçar um novo render a cada segundo.
function useCountdown(deadline: number | null): number | null {
  const [, setTick] = useState(0)
  useEffect(() => {
    if (deadline === null) return
    const t = setInterval(() => setTick((n) => n + 1), 1000)
    return () => clearInterval(t)
  }, [deadline])
  if (deadline === null) return null
  return Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
}

const formatSeconds = (s: number) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
