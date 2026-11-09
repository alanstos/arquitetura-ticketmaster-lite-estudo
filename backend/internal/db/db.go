// Package db cuida da conexão, migrations e seed do Postgres.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed seed.sql
var seedSQL string

// Connect abre o pool, tentando novamente enquanto o Postgres sobe.
func Connect(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		log.Info("aguardando postgres", "tentativa", attempt, "erro", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("não foi possível conectar ao postgres: %w", lastErr)
}

// Migrate aplica, em ordem, os arquivos migrations/*.sql ainda não aplicados.
// Para alterar o schema, crie um NOVO arquivo (ex.: 002_xxx.sql); nunca edite um já aplicado.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		// Exec sem argumentos usa o protocolo simples => aceita múltiplos statements.
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		log.Info("migration aplicada", "version", version)
	}
	return nil
}

// Seed popula dados mock se ainda não houver eventos.
func Seed(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		log.Info("seed ignorado: já existem eventos", "eventos", count)
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, seedSQL); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Info("seed aplicado")
	return nil
}
