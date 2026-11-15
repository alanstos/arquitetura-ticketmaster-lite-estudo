package search

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// OutboxChannel é o canal do NOTIFY disparado pelos triggers (migration 003).
const OutboxChannel = "search_outbox"

const (
	batchSize = 500
	// pollInterval: rede de segurança caso uma notificação se perca (ex.: o
	// LISTEN caiu e reconectou). O caminho normal é ser acordado pelo NOTIFY.
	pollInterval = 5 * time.Second
	maxBackoff   = 30 * time.Second
)

var tracer = otel.Tracer("ticketmaster-lite/search")

// Indexer mantém o Elasticsearch em dia com o Postgres consumindo o search_outbox:
//
//	events/venues --trigger--> search_outbox --NOTIFY--> Indexer --_bulk--> ES
//
// Entrega "pelo menos uma vez": a linha do outbox só é apagada depois que o ES
// confirmou o _bulk. Se o processo morrer no meio, a linha é reprocessada, o que
// é inofensivo (o documento é sempre o estado atual do evento).
type Indexer struct {
	es          *Client
	pool        *pgxpool.Pool
	databaseURL string
	log         *slog.Logger
	wake        chan struct{}
	indexReady  bool // EnsureIndex já rodou com sucesso (só usado pela goroutine run)
}

func NewIndexer(es *Client, pool *pgxpool.Pool, databaseURL string, log *slog.Logger) *Indexer {
	return &Indexer{es: es, pool: pool, databaseURL: databaseURL, log: log, wake: make(chan struct{}, 1)}
}

// Run bloqueia até ctx ser cancelado.
func (ix *Indexer) Run(ctx context.Context) {
	go ix.listen(ctx)
	ix.poke()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-ix.wake:
		case <-ticker.C:
		}
		n, err := ix.drain(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// ES fora do ar, índice apagado...: o outbox segura as mudanças até
			// voltar. Reverifica o índice na próxima tentativa.
			ix.indexReady = false
			ix.log.Warn("indexação da busca falhou; tentando de novo", "erro", err, "em", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			ix.poke()
			continue
		}
		backoff = time.Second
		if n > 0 {
			ix.log.Info("busca: eventos reindexados", "quantidade", n)
		}
	}
}

func (ix *Indexer) poke() {
	select {
	case ix.wake <- struct{}{}:
	default: // já há um "acorde" pendente
	}
}

// drain processa o outbox em lotes até esvaziá-lo. Devolve quantos eventos
// foram (re)indexados ou removidos.
func (ix *Indexer) drain(ctx context.Context) (int, error) {
	if !ix.indexReady {
		created, err := ix.es.EnsureIndex(ctx)
		if err != nil {
			return 0, fmt.Errorf("garantir índice: %w", err)
		}
		if created {
			// Índice novo (primeira subida ou volume do ES apagado): enfileira
			// todos os eventos. Eventos passados também vão; o filtro de data da
			// busca os esconde.
			tag, err := ix.pool.Exec(ctx, `INSERT INTO search_outbox (event_id) SELECT id FROM events`)
			if err != nil {
				return 0, fmt.Errorf("enfileirar reindexação completa: %w", err)
			}
			ix.log.Info("índice de busca criado; reindexação completa enfileirada",
				"index", indexName, "alias", Alias, "eventos", tag.RowsAffected())
		}
		ix.indexReady = true
	}

	total := 0
	for {
		n, err := ix.processBatch(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// processBatch pega um lote do outbox, manda para o ES e só então apaga as
// linhas, tudo na mesma transação. FOR UPDATE SKIP LOCKED permite rodar mais de
// uma réplica do backend: cada uma pega linhas diferentes, sem esperar.
func (ix *Indexer) processBatch(ctx context.Context) (n int, err error) {
	ctx, span := tracer.Start(ctx, "search.index_batch")
	defer func() {
		span.SetAttributes(attribute.Int("search.events", n))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	tx, err := ix.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // no-op após Commit

	rows, err := tx.Query(ctx, `
SELECT id, event_id FROM search_outbox
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED`, batchSize)
	if err != nil {
		return 0, err
	}
	var outboxIDs []int64
	seen := map[int64]bool{}
	var eventIDs []int64
	for rows.Next() {
		var oid, eid int64
		if err := rows.Scan(&oid, &eid); err != nil {
			rows.Close()
			return 0, err
		}
		outboxIDs = append(outboxIDs, oid)
		if !seen[eid] { // o mesmo evento pode ter mudado várias vezes
			seen[eid] = true
			eventIDs = append(eventIDs, eid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(outboxIDs) == 0 {
		return 0, nil
	}

	docs, err := loadDocs(ctx, tx, eventIDs)
	if err != nil {
		return 0, err
	}
	var deletes []int64
	for _, id := range eventIDs {
		if _, ok := docs[id]; !ok {
			deletes = append(deletes, id) // evento apagado do Postgres
		}
	}
	list := make([]Doc, 0, len(docs))
	for _, d := range docs {
		list = append(list, d)
	}
	span.AddEvent("bulk", trace.WithAttributes(
		attribute.Int("search.upserts", len(list)), attribute.Int("search.deletes", len(deletes))))

	// Prazo curto: a transação (e os locks das linhas do outbox) fica aberta
	// enquanto esperamos o ES.
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = ix.es.Bulk(bctx, list, deletes)
	cancel()
	if err != nil {
		return 0, err // rollback: as linhas continuam no outbox
	}
	if _, err := tx.Exec(ctx, `DELETE FROM search_outbox WHERE id = ANY($1)`, outboxIDs); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(eventIDs), nil
}

func loadDocs(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]Doc, error) {
	rows, err := tx.Query(ctx, `
SELECT e.id, e.name, e.description, e.starts_at, v.name, v.city
FROM events e
JOIN venues v ON v.id = e.venue_id
WHERE e.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := map[int64]Doc{}
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.StartsAt, &d.VenueName, &d.City); err != nil {
			return nil, err
		}
		docs[d.ID] = d
	}
	return docs, rows.Err()
}

// listen mantém uma conexão dedicada com LISTEN e reconecta com backoff se ela
// cair (mesmo desenho do internal/live). Cada notificação só "acorda" o loop.
func (ix *Indexer) listen(ctx context.Context) {
	backoff := time.Second
	for {
		err := ix.listenOnce(ctx, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		ix.log.Warn("LISTEN do outbox de busca caiu; reconectando", "erro", err, "em", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (ix *Indexer) listenOnce(ctx context.Context, connected func()) error {
	conn, err := pgx.Connect(ctx, ix.databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+OutboxChannel); err != nil {
		return err
	}
	connected()
	ix.poke() // pode ter chegado algo enquanto estávamos desconectados
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		ix.poke()
	}
}
