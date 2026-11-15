// Package search implementa a busca de eventos com Elasticsearch (ADR-013):
// tolerante a erros de digitação ("jota qest"), por prefixo ("casa" -> casarão)
// e por radical em português ("casas" -> casa).
//
// O Elasticsearch NÃO é a fonte da verdade: ele só devolve os IDs dos eventos
// em ordem de relevância; os dados (inclusive assentos livres) vêm do Postgres.
// Se o ES cair, o store volta para a busca ILIKE no Postgres.
//
// Cliente REST puro (net/http + encoding/json), sem biblioteca: deixa explícitas
// as chamadas da API do ES e não puxa dependências que brigam com o Go 1.23.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"ticketmaster-lite/backend/internal/store"
)

const (
	// Alias é o nome usado nas buscas e na indexação. Ele aponta para um índice
	// versionado (indexName): para mudar o mapping no futuro, cria-se events_v2,
	// reindexa e troca o alias, sem downtime.
	Alias     = "events"
	indexName = "events_v1"

	// searchTimeout: busca é síncrona, na frente do usuário. Se o ES demorar mais
	// que isso, vale mais responder com o fallback do Postgres.
	searchTimeout = 500 * time.Millisecond
	// breakerCooldown: após uma falha, pula o ES por esse tempo (mesma ideia do
	// circuit breaker do seat gate, ADR-010).
	breakerCooldown = 5 * time.Second
	// maxResults: busca por relevância devolve "os melhores", não tudo.
	maxResults = 50
)

// Client fala com o Elasticsearch e implementa store.Searcher.
type Client struct {
	baseURL   string
	http      *http.Client
	openUntil atomic.Int64 // circuit breaker: UnixNano até quando pular o ES
}

// New cria o cliente (ex.: "http://elasticsearch:9200"). O transporte otelhttp
// gera um span por chamada ao ES, filho do span da requisição HTTP.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport,
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return "elasticsearch " + r.Method + " " + r.URL.Path
				})),
			Timeout: 30 * time.Second, // teto geral; cada chamada usa um ctx mais curto
		},
	}
}

// esError é o erro devolvido quando o ES responde com status >= 400.
type esError struct {
	Status int
	Body   string
}

func (e *esError) Error() string {
	return fmt.Sprintf("elasticsearch respondeu %d: %s", e.Status, e.Body)
}

// do envia uma requisição JSON e decodifica a resposta em out (se não for nil).
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte: // corpo já serializado (ex.: _bulk em NDJSON)
		rd = bytes.NewReader(b)
		contentType = "application/x-ndjson"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	if rd != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &esError{Status: resp.StatusCode, Body: string(msg)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping verifica se o cluster responde (usado no readiness: fora => "degraded").
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_cluster/health", nil, nil)
}

// EnsureIndex cria o índice (com analyzers e mapping) e o alias, se ainda não
// existirem. created=true indica índice novo e vazio: o chamador deve reindexar tudo.
func (c *Client) EnsureIndex(ctx context.Context) (created bool, err error) {
	err = c.do(ctx, http.MethodHead, "/_alias/"+Alias, nil, nil)
	if err == nil {
		return false, nil
	}
	var esErr *esError
	if !errors.As(err, &esErr) || esErr.Status != http.StatusNotFound {
		return false, err
	}
	body := map[string]any{
		"settings": indexSettings,
		"mappings": indexMappings,
		"aliases":  map[string]any{Alias: map[string]any{}},
	}
	if err := c.do(ctx, http.MethodPut, "/"+indexName, body, nil); err != nil {
		// Outra réplica do backend pode ter criado o índice ao mesmo tempo.
		if errors.As(err, &esErr) && strings.Contains(esErr.Body, "resource_already_exists_exception") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SearchEventIDs devolve os IDs dos eventos que casam com f.Keyword, do mais
// relevante para o menos relevante (empate: o que acontece antes).
func (c *Client) SearchEventIDs(ctx context.Context, f store.SearchFilter) ([]int64, error) {
	if time.Now().UnixNano() < c.openUntil.Load() {
		return nil, store.ErrSearchUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	var resp struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := c.do(ctx, http.MethodPost, "/"+Alias+"/_search", buildQuery(f), &resp); err != nil {
		c.openUntil.Store(time.Now().Add(breakerCooldown).UnixNano())
		return nil, err
	}
	ids := make([]int64, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		id, err := strconv.ParseInt(h.ID, 10, 64)
		if err != nil {
			continue // documento estranho no índice: ignora
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Doc é o documento de busca de um evento (desnormalizado: inclui o venue).
type Doc struct {
	ID          int64     `json:"-"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	VenueName   string    `json:"venue_name"`
	City        string    `json:"city"`
	StartsAt    time.Time `json:"starts_at"`
}

// Bulk indexa (upsert) docs e apaga deletes numa única chamada _bulk.
func (c *Client) Bulk(ctx context.Context, docs []Doc, deletes []int64) error {
	if len(docs) == 0 && len(deletes) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // Encode já termina cada linha com \n (NDJSON)
	for _, d := range docs {
		_ = enc.Encode(map[string]any{"index": map[string]any{"_index": Alias, "_id": strconv.FormatInt(d.ID, 10)}})
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	for _, id := range deletes {
		_ = enc.Encode(map[string]any{"delete": map[string]any{"_index": Alias, "_id": strconv.FormatInt(id, 10)}})
	}

	var resp struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodPost, "/_bulk", buf.Bytes(), &resp); err != nil {
		return err
	}
	if !resp.Errors {
		return nil
	}
	// _bulk responde 200 mesmo com falhas por item. Apagar algo que não existe
	// (404) não é erro para nós; o resto é.
	for _, item := range resp.Items {
		for op, r := range item {
			if r.Status >= 300 && !(op == "delete" && r.Status == http.StatusNotFound) {
				return fmt.Errorf("_bulk: %s falhou (%d): %s", op, r.Status, r.Error)
			}
		}
	}
	return nil
}
