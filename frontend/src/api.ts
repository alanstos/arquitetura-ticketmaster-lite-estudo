// Cliente da API do backend Go. Todas as rotas ficam sob /api (proxy do Vite).

export type Venue = {
  id: number
  name: string
  city: string
  address: string
  rows: number
  cols: number
}

export type EventSummary = {
  id: number
  slug: string
  name: string
  description: string
  starts_at: string
  price_cents: number
  venue: Venue
  available_seats: number
  total_seats: number
}

export type SeatStatus = 'AVAILABLE' | 'RESERVED' | 'SOLD'

export type Seat = {
  ticket_id: number
  row: string
  number: number
  label: string
  status: SeatStatus
}

export type EventDetail = EventSummary & { seats: Seat[] }

export type Booking = {
  id: string
  event_id: number
  event_slug: string
  customer_name: string
  customer_email: string
  status: 'PENDING' | 'CONFIRMED' | 'EXPIRED'
  seats: string[]
  ticket_ids: number[]
  total_cents: number
  expires_at: string
  seconds_remaining: number // tempo restante do hold, medido pelo relógio do servidor
  created_at: string
  confirmed_at?: string
}

export class ApiError extends Error {
  constructor(public status: number, message: string, public body: unknown) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  })
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    throw new ApiError(res.status, body?.error ?? `Erro HTTP ${res.status}`, body)
  }
  return body as T
}

export type SearchParams = { keyword?: string; startDate?: string; endDate?: string }

export function listEvents() {
  return request<EventSummary[]>('/events')
}

export function searchEvents(p: SearchParams) {
  const qs = new URLSearchParams()
  if (p.keyword) qs.set('keyword', p.keyword)
  if (p.startDate) qs.set('startDate', p.startDate)
  if (p.endDate) qs.set('endDate', p.endDate)
  return request<EventSummary[]>(`/search?${qs}`)
}

export function getEvent(slug: string) {
  return request<EventDetail>(`/events/${encodeURIComponent(slug)}`)
}

export function reserve(input: {
  event_id: number
  ticket_ids: number[]
  customer_name: string
  customer_email: string
}) {
  return request<Booking>('/reserve', { method: 'POST', body: JSON.stringify(input) })
}

export function confirmBooking(bookingId: string) {
  return request<Booking>('/booking', {
    method: 'POST',
    body: JSON.stringify({ booking_id: bookingId }),
  })
}

export const formatPrice = (cents: number) =>
  (cents / 100).toLocaleString('pt-BR', { style: 'currency', currency: 'BRL' })

export const formatDate = (iso: string) =>
  new Date(iso).toLocaleString('pt-BR', {
    timeZone: 'America/Sao_Paulo',
    dateStyle: 'full',
    timeStyle: 'short',
  })
