import { FormEvent, useCallback, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  ApiError,
  Booking,
  EventDetail,
  confirmBooking,
  formatDate,
  formatPrice,
  getEvent,
  reserve,
} from '../api'
import SeatMap from '../components/SeatMap'

const POLL_MS = 5000

// Fluxo: selecionar assentos -> POST /reserve (hold com prazo) -> POST /booking (confirma).
export default function EventPage() {
  const { slug = '' } = useParams()
  const [event, setEvent] = useState<EventDetail | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [booking, setBooking] = useState<Booking | null>(null)
  const [message, setMessage] = useState<{ kind: 'error' | 'success' | 'info'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      const ev = await getEvent(slug)
      setEvent(ev)
      setError(null)
      // Remove da seleção assentos que outra pessoa pegou nesse meio tempo.
      setSelected((prev) => {
        const free = new Set(ev.seats.filter((s) => s.status === 'AVAILABLE').map((s) => s.ticket_id))
        const next = new Set([...prev].filter((id) => free.has(id)))
        return next.size === prev.size ? prev : next
      })
    } catch (e) {
      setError(e instanceof ApiError && e.status === 404 ? 'Evento não encontrado.' : (e as Error).message)
    }
  }, [slug])

  // Polling simples para refletir compras de outros usuários.
  useEffect(() => {
    load()
    const t = setInterval(load, POLL_MS)
    return () => clearInterval(t)
  }, [load])

  const secondsLeft = useCountdown(booking?.status === 'PENDING' ? booking.expires_at : null)
  useEffect(() => {
    if (booking?.status === 'PENDING' && secondsLeft === 0) {
      setBooking(null)
      setMessage({ kind: 'error', text: 'O tempo da sua reserva acabou. Os assentos foram liberados.' })
      load()
    }
  }, [secondsLeft, booking, load])

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
      load()
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
      load()
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
          {message && <p className={`alert ${message.kind}`}>{message.text}</p>}

          {pending && booking && (
            <>
              <h3>Sua reserva</h3>
              <p>Assentos: {booking.seats.join(', ')}</p>
              <p>Total: {formatPrice(booking.total_cents)}</p>
              <p className="countdown">
                Expira em {Math.floor(secondsLeft / 60)}:{String(secondsLeft % 60).padStart(2, '0')}
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

function useCountdown(expiresAt: string | null) {
  const calc = () => (expiresAt ? Math.max(0, Math.round((Date.parse(expiresAt) - Date.now()) / 1000)) : 0)
  const [left, setLeft] = useState(calc)
  useEffect(() => {
    setLeft(calc())
    if (!expiresAt) return
    const t = setInterval(() => setLeft(calc()), 1000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expiresAt])
  return left
}
