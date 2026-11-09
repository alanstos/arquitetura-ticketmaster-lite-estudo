import { Seat } from '../api'

type Props = {
  seats: Seat[]
  cols: number
  selected: Set<number>
  mine: Set<number> // assentos que pertencem à reserva em andamento deste usuário
  disabled?: boolean
  onToggle: (ticketId: number) => void
}

export default function SeatMap({ seats, cols, selected, mine, disabled, onToggle }: Props) {
  return (
    <div className="seatmap">
      <div className="stage">PALCO</div>
      <div className="grid" style={{ gridTemplateColumns: `repeat(${cols}, 1fr)` }}>
        {seats.map((s) => {
          const isMine = mine.has(s.ticket_id)
          const isSelected = selected.has(s.ticket_id)
          const cls = isMine ? 'mine' : isSelected ? 'selected' : s.status.toLowerCase()
          const clickable = !disabled && s.status === 'AVAILABLE'
          return (
            <button
              key={s.ticket_id}
              className={`seat ${cls}`}
              disabled={!clickable}
              onClick={() => onToggle(s.ticket_id)}
              title={`${s.label} — ${statusLabel(s.status, isMine)}`}
            >
              {s.label}
            </button>
          )
        })}
      </div>
      <div className="legend">
        <span><i className="seat available" /> Livre</span>
        <span><i className="seat selected" /> Selecionado</span>
        <span><i className="seat mine" /> Sua reserva</span>
        <span><i className="seat reserved" /> Reservado</span>
        <span><i className="seat sold" /> Vendido</span>
      </div>
    </div>
  )
}

function statusLabel(status: Seat['status'], mine: boolean) {
  if (mine) return 'sua reserva'
  return { AVAILABLE: 'livre', RESERVED: 'reservado', SOLD: 'vendido' }[status]
}
