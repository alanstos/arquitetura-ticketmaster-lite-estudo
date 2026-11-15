-- Notificação de mudança no mapa de assentos (ADR-012).
--
-- Toda escrita relevante em tickets dispara pg_notify('seat_changes', event_id).
-- Propriedades do NOTIFY que o desenho usa:
--   * é transacional: só é entregue no COMMIT (rollback => nada é enviado);
--   * notificações idênticas (mesmo canal + payload) na mesma transação são
--     colapsadas em uma só: reservar 3 assentos gera UMA notificação.
-- O backend escuta com LISTEN numa conexão dedicada (internal/live) e empurra o
-- novo mapa para os navegadores via SSE.
CREATE FUNCTION notify_seat_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('seat_changes', NEW.event_id::text);
    RETURN NULL; -- AFTER trigger: valor de retorno é ignorado
END;
$$ LANGUAGE plpgsql;

-- "UPDATE OF ..." só dispara quando alguma dessas colunas aparece no SET.
CREATE TRIGGER tickets_notify_seat_change
AFTER INSERT OR UPDATE OF status, booking_id, reserved_until ON tickets
FOR EACH ROW EXECUTE FUNCTION notify_seat_change();
