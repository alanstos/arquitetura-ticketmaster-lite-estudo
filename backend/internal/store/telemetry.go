package store

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instrumentação de negócio (ADR-011). Métricas HTTP dizem "quantos 409";
// estas dizem POR QUE: o gate do Redis recusou ou o Postgres recusou.
//
// tracer/meter vêm dos providers globais: se o OTel estiver desligado, são no-op.
var tracer = otel.Tracer("ticketmaster-lite/store")

type instruments struct {
	reservations metric.Int64Counter // atributo result
	gateFallback metric.Int64Counter // atributo reason
	bookings     metric.Int64Counter // atributo result
}

func newInstruments() instruments {
	m := otel.Meter("ticketmaster-lite/store")
	// Erros só ocorrem com nome/opção inválidos (bug de programação); o OTel
	// devolve um instrumento no-op utilizável mesmo nesse caso.
	r, _ := m.Int64Counter("ticketmaster.reservations",
		metric.WithDescription("Tentativas de reserva por resultado: created, conflict, gate_rejected, invalid, error"))
	f, _ := m.Int64Counter("ticketmaster.seat_gate.fallback",
		metric.WithDescription("Reservas que seguiram só com o Postgres porque o seat gate (Redis) falhou ou estava em pausa"))
	b, _ := m.Int64Counter("ticketmaster.bookings",
		metric.WithDescription("Confirmações de booking por resultado: confirmed, expired, not_found, error"))
	return instruments{reservations: r, gateFallback: f, bookings: b}
}

// Resultados de reserva (valores do atributo "result").
const (
	resultCreated      = "created"
	resultConflict     = "conflict"      // Postgres: assento já tomado/travado (SKIP LOCKED)
	resultGateRejected = "gate_rejected" // Redis: hold de outro cliente, nem chegou ao banco
	resultInvalid      = "invalid"
	resultError        = "error"
)

func reserveResult(err error, byGate bool) string {
	var seatErr *SeatUnavailableError
	var valErr *ValidationError
	switch {
	case err == nil:
		return resultCreated
	case errors.As(err, &seatErr) && byGate:
		return resultGateRejected
	case errors.As(err, &seatErr):
		return resultConflict
	case errors.As(err, &valErr):
		return resultInvalid
	default:
		return resultError
	}
}

func bookingResult(err error) string {
	switch {
	case err == nil:
		return "confirmed"
	case errors.Is(err, ErrBookingExpired):
		return "expired"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	default:
		return resultError
	}
}

// endSpan grava o resultado no span e só marca erro de verdade como erro:
// 409/400 são respostas esperadas do domínio, não falhas do sistema.
func endSpan(span trace.Span, result string, err error) {
	span.SetAttributes(attribute.String("result", result))
	if result == resultError {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func (s *Store) countFallback(ctx context.Context, reason string) {
	s.m.gateFallback.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	trace.SpanFromContext(ctx).AddEvent("seat_gate.fallback", trace.WithAttributes(attribute.String("reason", reason)))
}
