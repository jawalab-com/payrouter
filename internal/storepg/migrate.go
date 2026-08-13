// Package storepg is the durable PostgreSQL implementation of store.Store for
// the payment facade. It owns its own schema (facade) and migration history
// (facade.schema_migrations) within the shared billing database.
package storepg

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stripe-compatible-facade/migrations"
)

// Migrate applies every embedded facade migration not yet recorded in
// facade.schema_migrations, oldest first, each in its own transaction. It
// creates the facade schema and history table if absent.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS facade;
		CREATE TABLE IF NOT EXISTS facade.schema_migrations (
			version    integer PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
	`); err != nil {
		return fmt.Errorf("ensure facade.schema_migrations: %w", err)
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	for _, f := range files {
		if applied[f.version] {
			continue
		}
		body, err := migrations.Files.ReadFile(f.name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f.name, err)
		}
		if err := applyOne(ctx, pool, f.version, f.name, string(body)); err != nil {
			return err
		}
		applied[f.version] = true
	}
	return nil
}

type migFile struct {
	version int
	name    string
}

func migrationFiles() ([]migFile, error) {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}
	var out []migFile
	versions := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("unexpected non-sql migration file: %s", name)
		}
		prefix := strings.SplitN(name, "_", 2)[0]
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %s: version prefix %q is not an integer", name, prefix)
		}
		if previous, exists := versions[v]; exists {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", v, previous, name)
		}
		versions[v] = name
		out = append(out, migFile{version: v, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM facade.schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, version int, name, body string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", name, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO facade.schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, version); err != nil {
		return fmt.Errorf("record %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// ConnectPool parses the database URL, pins search_path to the facade schema for
// every pooled connection, and returns a ready pool.
func ConnectPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "facade"
	cfg.MaxConns = 8
	cfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return pool, nil
}

// txKey brands the pgx.Tx stashed in a context by Store.RunInTx so capability
// methods can run against the in-flight transaction.
type txKey struct{}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}
