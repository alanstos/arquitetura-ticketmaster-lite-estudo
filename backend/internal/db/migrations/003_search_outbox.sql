-- Outbox transacional para o índice de busca no Elasticsearch (ADR-013).
--
-- O Postgres continua sendo a fonte da verdade; o Elasticsearch é só um índice
-- derivado. Toda escrita em events (ou em dados do venue que aparecem na busca)
-- grava, NA MESMA TRANSAÇÃO, uma linha "este evento mudou" nesta tabela:
--   * COMMIT  => a linha existe e o indexer (internal/search) vai reindexar o evento;
--   * ROLLBACK => a linha some junto, nada é indexado.
-- Isso evita o "dual write" (gravar no banco e no ES separadamente), em que uma
-- das duas escritas pode falhar e deixar os dois lados divergentes para sempre.
--
-- A linha guarda só o event_id, não a operação: o indexer lê o estado ATUAL do
-- evento; se ele existir, faz upsert no índice, se não existir mais, apaga.
-- Reprocessar a mesma linha é inofensivo (idempotente), então a entrega pode ser
-- "pelo menos uma vez".
CREATE TABLE search_outbox (
    id         BIGSERIAL PRIMARY KEY,
    event_id   BIGINT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE FUNCTION enqueue_event_search() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        INSERT INTO search_outbox (event_id) VALUES (OLD.id);
    ELSE
        INSERT INTO search_outbox (event_id) VALUES (NEW.id);
    END IF;
    -- Acorda o indexer no COMMIT (notificações iguais na mesma transação são
    -- colapsadas em uma só). Se a notificação se perder, o poll periódico pega.
    PERFORM pg_notify('search_outbox', '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_search_outbox
AFTER INSERT OR UPDATE OR DELETE ON events
FOR EACH ROW EXECUTE FUNCTION enqueue_event_search();

-- Nome/cidade do venue fazem parte do documento de busca do evento: mudou o
-- venue, reindexa todos os eventos dele.
CREATE FUNCTION enqueue_venue_search() RETURNS trigger AS $$
BEGIN
    INSERT INTO search_outbox (event_id) SELECT id FROM events WHERE venue_id = NEW.id;
    PERFORM pg_notify('search_outbox', '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER venues_search_outbox
AFTER UPDATE OF name, city ON venues
FOR EACH ROW EXECUTE FUNCTION enqueue_venue_search();
