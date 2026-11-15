// Package api expõe os endpoints HTTP (net/http com roteamento do Go 1.22+).
//
// Responsabilidade desta camada: ler/validar a requisição (formato, campos
// obrigatórios), chamar o store e traduzir o resultado/erro para status HTTP.
// Regras de negócio e concorrência NÃO ficam aqui — ficam em internal/store.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"ticketmaster-lite/backend/internal/store"
)

// handler agrupa as dependências compartilhadas pelos endpoints.
type handler struct {
	st  *store.Store
	log *slog.Logger
}

// NewRouter registra as rotas e devolve o handler HTTP já envolvido pelos
// middlewares de observabilidade: otelhttp (span + métricas por requisição) por
// fora e observe (log de acesso + nome da rota no span/métricas) por dentro.
func NewRouter(st *store.Store, log *slog.Logger) http.Handler {
	h := &handler{st: st, log: log}
	mux := http.NewServeMux()
	// Desde o Go 1.22 o ServeMux aceita o método no padrão ("GET /...") e
	// curingas no caminho ({idOrSlug}), lidos depois com r.PathValue.
	// Método diferente do registrado gera 405 automaticamente.
	// Liveness: o processo está de pé e respondendo (não olha dependências).
	// É o que o healthcheck do docker compose usa.
	mux.HandleFunc("GET /api/health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: consegue atender? Pinga Postgres e Redis. /api/health é alias.
	mux.HandleFunc("GET /api/health/ready", h.ready)
	mux.HandleFunc("GET /api/health", h.ready)
	mux.HandleFunc("GET /api/events", h.listEvents)
	mux.HandleFunc("GET /api/events/{idOrSlug}", h.getEvent)
	mux.HandleFunc("GET /api/search", h.search)
	mux.HandleFunc("POST /api/reserve", h.reserve)        // fase 1: segura os assentos (hold com TTL)
	mux.HandleFunc("POST /api/booking", h.confirmBooking) // fase 2: confirma/paga a reserva
	mux.HandleFunc("GET /api/booking/{id}", h.getBooking)
	mux.HandleFunc("GET /api/venues", h.listVenues)
	// Relatório administrativo. ATENÇÃO: sem autenticação (projeto de estudo) —
	// expõe nome/e-mail dos compradores. Ver docs/decisions.md (ADR-009).
	mux.HandleFunc("GET /api/admin/tickets", h.adminTickets)
	return otelhttp.NewHandler(observe(log, mux), "http.server",
		// Health checks são chamados o tempo todo e não interessam como trace/métrica.
		otelhttp.WithFilter(func(r *http.Request) bool { return !isHealth(r) }),
	)
}

// GET /api/health/ready — 200 "ok"/"degraded" ou 503 "down" (ver store.Health).
func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	rep := h.st.Health(r.Context())
	status := http.StatusOK
	if rep.Status == store.HealthDown {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, rep)
}

// GET /api/events — é a busca sem nenhum filtro (todos os eventos futuros).
func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := h.st.SearchEvents(r.Context(), store.SearchFilter{})
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// GET /api/search?keyword=&startDate=YYYY-MM-DD&endDate=YYYY-MM-DD
// Todos os parâmetros são opcionais; parâmetro ausente = filtro não aplicado.
func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SearchFilter{Keyword: strings.TrimSpace(q.Get("keyword"))}
	var ok bool
	if f.StartDate, f.EndDate, ok = parseDateRange(w, q); !ok {
		return
	}
	events, err := h.st.SearchEvents(r.Context(), f)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// GET /api/events/{idOrSlug} — aceita slug (jota-quest-rio-de-janeiro) ou id numérico.
func (h *handler) getEvent(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("idOrSlug")
	// Erro de conversão ignorado de propósito: se não for número, id fica 0
	// (nenhum evento tem id 0) e a busca acontece só pelo slug.
	id, _ := strconv.ParseInt(key, 10, 64)
	ev, err := h.st.GetEvent(r.Context(), key, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// reserveRequest é o corpo esperado em POST /api/reserve.
type reserveRequest struct {
	EventID       int64   `json:"event_id"`
	TicketIDs     []int64 `json:"ticket_ids"` // ids dos assentos (tickets) escolhidos no mapa
	CustomerName  string  `json:"customer_name"`
	CustomerEmail string  `json:"customer_email"`
}

// POST /api/reserve — segura os assentos por RESERVATION_TTL e cria um booking PENDING.
// É aqui que a disputa entre usuários pelo mesmo assento é decidida (ver store.Reserve).
func (h *handler) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validações de formato ficam no handler; validações que dependem do
	// banco (assento existe, pertence ao evento, está livre) ficam no store.
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
	// Mapeamento erro do store -> status HTTP. errors.As é usado porque esses
	// erros são tipos (structs) que carregam dados, não valores sentinela.
	var vErr *store.ValidationError
	var sErr *store.SeatUnavailableError
	switch {
	case errors.As(err, &vErr):
		writeError(w, http.StatusBadRequest, vErr.Msg)
	case errors.As(err, &sErr):
		// 409 Conflict: outra pessoa reservou/comprou antes. Devolvemos quais
		// assentos falharam para o frontend poder destacar e pedir outra escolha.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                  "um ou mais assentos já foram reservados ou vendidos",
			"unavailable_ticket_ids": sErr.TicketIDs,
		})
	case err != nil:
		h.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, b)
	}
}

// confirmRequest é o corpo esperado em POST /api/booking.
type confirmRequest struct {
	BookingID string `json:"booking_id"`
}

// POST /api/booking — confirma (simula pagamento) um booking PENDING.
// Idempotente: confirmar de novo um booking já CONFIRMED devolve 200.
func (h *handler) confirmBooking(w http.ResponseWriter, r *http.Request) {
	var req confirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Valida o formato do UUID antes de ir ao banco: um id malformado faria
	// o Postgres falhar na conversão para UUID e viraria um 500 indevido.
	if !uuidRe.MatchString(req.BookingID) {
		writeError(w, http.StatusBadRequest, "booking_id inválido")
		return
	}
	b, err := h.st.ConfirmBooking(r.Context(), req.BookingID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "booking não encontrado")
	case errors.Is(err, store.ErrBookingExpired):
		// 410 Gone: a reserva existiu, mas o prazo (TTL) passou e os assentos
		// foram liberados — o usuário precisa reservar novamente.
		writeError(w, http.StatusGone, "a reserva expirou; selecione os assentos novamente")
	case err != nil:
		h.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, b)
	}
}

// GET /api/booking/{id} — consulta um booking (status, assentos, prazo).
func (h *handler) getBooking(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) { // mesmo motivo do confirmBooking: evitar 500 por UUID malformado
		writeError(w, http.StatusBadRequest, "booking_id inválido")
		return
	}
	b, err := h.st.GetBooking(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking não encontrado")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// GET /api/venues — lista os locais (usado no filtro do relatório /admin).
func (h *handler) listVenues(w http.ResponseWriter, r *http.Request) {
	venues, err := h.st.ListVenues(r.Context())
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, venues)
}

// GET /api/admin/tickets?venueId=&startDate=YYYY-MM-DD&endDate=YYYY-MM-DD
// Relatório: um item por assento (vendido, reservado ou disponível) com o
// comprador. Diferente da busca pública, inclui eventos que já aconteceram.
func (h *handler) adminTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.AdminFilter
	if v := q.Get("venueId"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "venueId inválido")
			return
		}
		f.VenueID = id
	}
	var ok bool
	if f.StartDate, f.EndDate, ok = parseDateRange(w, q); !ok {
		return
	}
	tickets, err := h.st.AdminTickets(r.Context(), f)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tickets)
}

// ---- helpers ----

// parseDateRange lê startDate/endDate da query string. As datas são validadas
// aqui para responder 400 com mensagem clara, em vez de deixar o Postgres
// falhar com erro de conversão (500). Se falhar, já responde 400 e devolve ok=false.
func parseDateRange(w http.ResponseWriter, q url.Values) (start, end *time.Time, ok bool) {
	var err error
	if start, err = parseDate(q.Get("startDate")); err != nil {
		writeError(w, http.StatusBadRequest, "startDate inválido (use YYYY-MM-DD)")
		return nil, nil, false
	}
	if end, err = parseDate(q.Get("endDate")); err != nil {
		writeError(w, http.StatusBadRequest, "endDate inválido (use YYYY-MM-DD)")
		return nil, nil, false
	}
	if start != nil && end != nil && end.Before(*start) {
		writeError(w, http.StatusBadRequest, "endDate deve ser maior ou igual a startDate")
		return nil, nil, false
	}
	return start, end, true
}

// uuidRe valida o formato textual de um UUID (8-4-4-4-12 hexadecimais).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseDate converte "YYYY-MM-DD" em *time.Time. String vazia devolve nil,
// que o store interpreta como "sem filtro" (vira NULL no SQL).
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

// decodeJSON lê o corpo da requisição em dst. Se falhar, já responde 400 e
// devolve false — o handler só precisa fazer `if !decodeJSON(...) { return }`.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	// MaxBytesReader limita o corpo a 1 MB (proteção contra payloads enormes).
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	// Rejeita campos desconhecidos: um erro de digitação (ex.: "ticketIds")
	// vira 400 explícito em vez de ser ignorado silenciosamente.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return false
	}
	return true
}

// writeJSON serializa v como resposta. Os headers precisam ser definidos
// ANTES de WriteHeader — depois dele, alterações em w.Header() são ignoradas.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError padroniza o formato de erro da API: {"error": "mensagem"}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError registra o erro real no log, mas devolve ao cliente apenas
// uma mensagem genérica (não vazar detalhes de SQL/infra na resposta).
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "erro interno", "erro", err) // Context: leva o trace_id
	// Marca o span da requisição com o erro real (o cliente só vê "erro interno").
	trace.SpanFromContext(r.Context()).RecordError(err)
	writeError(w, http.StatusInternalServerError, "erro interno")
}

// statusRecorder embrulha o ResponseWriter para "espiar" o status code
// escrito pelo handler, já que http.ResponseWriter não expõe esse valor.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// observe é o middleware de log de acesso. Fica DENTRO do otelhttp para:
//   - logar com o context da requisição (que tem o span => trace_id no log);
//   - depois que o ServeMux escolheu a rota, ler r.Pattern (ex.: "GET /api/events/{idOrSlug}")
//     e usá-lo como nome do span e atributo http.route das métricas. Usar o caminho
//     real (/api/events/show-x) criaria uma série de métricas por slug (cardinalidade).
func observe(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// status começa em 200 porque, se o handler escrever o corpo sem chamar
		// WriteHeader, o net/http assume 200 implicitamente.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if isHealth(r) {
			return
		}

		// O ServeMux preenche r.Pattern no próprio *Request recebido aqui.
		route := "unmatched" // 404/405: não deixar o caminho bruto virar label
		if _, path, ok := strings.Cut(r.Pattern, " "); ok {
			route = path
		}
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
		if l, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			l.Add(attribute.String("http.route", route))
		}

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		log.Log(r.Context(), level, "http", "method", r.Method, "route", route, "path", r.URL.RequestURI(),
			"status", rec.status, "dur_ms", float64(time.Since(start).Microseconds())/1000)
	})
}

func isHealth(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/health") }
