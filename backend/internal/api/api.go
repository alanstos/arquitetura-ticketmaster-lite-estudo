// Package api expõe os endpoints HTTP (net/http com roteamento do Go 1.22+).
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ticketmaster-lite/backend/internal/store"
)

type handler struct {
	st  *store.Store
	log *slog.Logger
}

func NewRouter(st *store.Store, log *slog.Logger) http.Handler {
	h := &handler{st: st, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/events", h.listEvents)
	mux.HandleFunc("GET /api/events/{idOrSlug}", h.getEvent)
	mux.HandleFunc("GET /api/search", h.search)
	mux.HandleFunc("POST /api/reserve", h.reserve)
	mux.HandleFunc("POST /api/booking", h.confirmBooking)
	mux.HandleFunc("GET /api/booking/{id}", h.getBooking)
	return logging(log, mux)
}

func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := h.st.SearchEvents(r.Context(), store.SearchFilter{})
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// GET /api/search?keyword=&startDate=YYYY-MM-DD&endDate=YYYY-MM-DD
func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SearchFilter{Keyword: strings.TrimSpace(q.Get("keyword"))}
	var err error
	if f.StartDate, err = parseDate(q.Get("startDate")); err != nil {
		writeError(w, http.StatusBadRequest, "startDate inválido (use YYYY-MM-DD)")
		return
	}
	if f.EndDate, err = parseDate(q.Get("endDate")); err != nil {
		writeError(w, http.StatusBadRequest, "endDate inválido (use YYYY-MM-DD)")
		return
	}
	if f.StartDate != nil && f.EndDate != nil && f.EndDate.Before(*f.StartDate) {
		writeError(w, http.StatusBadRequest, "endDate deve ser maior ou igual a startDate")
		return
	}
	events, err := h.st.SearchEvents(r.Context(), f)
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// GET /api/events/{idOrSlug} — aceita slug (jota-quest-rio-de-janeiro) ou id numérico.
func (h *handler) getEvent(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("idOrSlug")
	id, _ := strconv.ParseInt(key, 10, 64)
	ev, err := h.st.GetEvent(r.Context(), key, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

type reserveRequest struct {
	EventID       int64   `json:"event_id"`
	TicketIDs     []int64 `json:"ticket_ids"`
	CustomerName  string  `json:"customer_name"`
	CustomerEmail string  `json:"customer_email"`
}

func (h *handler) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.CustomerName = strings.TrimSpace(req.CustomerName)
	req.CustomerEmail = strings.TrimSpace(req.CustomerEmail)
	if req.EventID <= 0 {
		writeError(w, http.StatusBadRequest, "event_id é obrigatório")
		return
	}
	if req.CustomerName == "" {
		writeError(w, http.StatusBadRequest, "customer_name é obrigatório")
		return
	}
	if _, err := mail.ParseAddress(req.CustomerEmail); err != nil {
		writeError(w, http.StatusBadRequest, "customer_email inválido")
		return
	}

	b, err := h.st.Reserve(r.Context(), store.ReserveInput{
		EventID: req.EventID, TicketIDs: req.TicketIDs,
		CustomerName: req.CustomerName, CustomerEmail: req.CustomerEmail,
	})
	var vErr *store.ValidationError
	var sErr *store.SeatUnavailableError
	switch {
	case errors.As(err, &vErr):
		writeError(w, http.StatusBadRequest, vErr.Msg)
	case errors.As(err, &sErr):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                  "um ou mais assentos já foram reservados ou vendidos",
			"unavailable_ticket_ids": sErr.TicketIDs,
		})
	case err != nil:
		h.internalError(w, err)
	default:
		writeJSON(w, http.StatusCreated, b)
	}
}

type confirmRequest struct {
	BookingID string `json:"booking_id"`
}

// POST /api/booking — confirma (simula pagamento) um booking PENDING.
func (h *handler) confirmBooking(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !uuidRe.MatchString(req.BookingID) {
		writeError(w, http.StatusBadRequest, "booking_id inválido")
		return
	}
	b, err := h.st.ConfirmBooking(r.Context(), req.BookingID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "booking não encontrado")
	case errors.Is(err, store.ErrBookingExpired):
		writeError(w, http.StatusGone, "a reserva expirou; selecione os assentos novamente")
	case err != nil:
		h.internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, b)
	}
}

func (h *handler) getBooking(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "booking_id inválido")
		return
	}
	b, err := h.st.GetBooking(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking não encontrado")
		return
	}
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ---- helpers ----

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) internalError(w http.ResponseWriter, err error) {
	h.log.Error("erro interno", "erro", err)
	writeError(w, http.StatusInternalServerError, "erro interno")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("http", "method", r.Method, "path", r.URL.RequestURI(), "status", rec.status, "dur", time.Since(start).String())
	})
}
