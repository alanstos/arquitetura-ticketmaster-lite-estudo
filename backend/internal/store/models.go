package store

import (
	"errors"
	"fmt"
	"time"
)

type Venue struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Address string `json:"address"`
	Rows    int    `json:"rows"`
	Cols    int    `json:"cols"`
}

type EventSummary struct {
	ID             int64     `json:"id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	StartsAt       time.Time `json:"starts_at"`
	PriceCents     int       `json:"price_cents"`
	Venue          Venue     `json:"venue"`
	AvailableSeats int       `json:"available_seats"`
	TotalSeats     int       `json:"total_seats"`
}

// Status efetivo do assento. RESERVED com reserved_until no passado é exposto como AVAILABLE.
const (
	SeatAvailable = "AVAILABLE"
	SeatReserved  = "RESERVED"
	SeatSold      = "SOLD"
)

type Seat struct {
	TicketID int64  `json:"ticket_id"`
	Row      string `json:"row"`
	Number   int    `json:"number"`
	Label    string `json:"label"`
	Status   string `json:"status"`
}

type EventDetail struct {
	EventSummary
	Seats []Seat `json:"seats"`
}

type SearchFilter struct {
	Keyword   string
	StartDate *time.Time // inclusivo, data no fuso America/Sao_Paulo
	EndDate   *time.Time // inclusivo
}

const (
	BookingPending   = "PENDING"
	BookingConfirmed = "CONFIRMED"
	BookingExpired   = "EXPIRED"
)

type Booking struct {
	ID            string    `json:"id"`
	EventID       int64     `json:"event_id"`
	EventSlug     string    `json:"event_slug"`
	CustomerName  string    `json:"customer_name"`
	CustomerEmail string    `json:"customer_email"`
	Status        string    `json:"status"`
	Seats         []string  `json:"seats"`
	TicketIDs     []int64   `json:"ticket_ids"`
	TotalCents    int       `json:"total_cents"`
	ExpiresAt     time.Time `json:"expires_at"`
	// SecondsRemaining é calculado com o relógio do banco. O frontend usa este
	// valor (e não expires_at) para o countdown, evitando depender de o relógio
	// do navegador estar sincronizado com o do servidor (comum no WSL2).
	SecondsRemaining int        `json:"seconds_remaining"`
	CreatedAt        time.Time  `json:"created_at"`
	ConfirmedAt      *time.Time `json:"confirmed_at,omitempty"`
}

type ReserveInput struct {
	EventID       int64
	TicketIDs     []int64
	CustomerName  string
	CustomerEmail string
}

var (
	ErrNotFound       = errors.New("not found")
	ErrBookingExpired = errors.New("reserva expirada")
)

// ValidationError indica entrada inválida (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// SeatUnavailableError indica que um ou mais assentos já foram reservados/vendidos (HTTP 409).
type SeatUnavailableError struct{ TicketIDs []int64 }

func (e *SeatUnavailableError) Error() string {
	return fmt.Sprintf("assento(s) indisponível(is): %v", e.TicketIDs)
}
