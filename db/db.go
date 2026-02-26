package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

//go:embed schema.sql
var schema string

// Pool is the application-wide connection pool.
var Pool *pgxpool.Pool

// Connect creates a connection pool from the given DATABASE_URL, verifies
// connectivity, and ensures the required schema exists.
func Connect(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return fmt.Errorf("ping database: %w", err)
	}

	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return fmt.Errorf("ensure schema: %w", err)
	}

	Pool = pool
	logger.Log.Info("Connected to PostgreSQL (schema verified)")
	return nil
}

// Close shuts down the connection pool.
func Close() {
	if Pool != nil {
		Pool.Close()
	}
}
