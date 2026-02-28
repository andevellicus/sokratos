package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

//go:embed schema.sql
var schema string

// Pool is the application-wide connection pool.
var Pool *pgxpool.Pool

// DBPoolConfig holds tunable connection pool parameters.
type DBPoolConfig struct {
	MaxConns          int
	MinConns          int
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// Connect creates a connection pool from the given DATABASE_URL, verifies
// connectivity, and ensures the required schema exists.
func Connect(ctx context.Context, databaseURL string, poolCfg DBPoolConfig) error {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse pool config: %w", err)
	}
	poolConfig.MaxConns = int32(poolCfg.MaxConns)
	poolConfig.MinConns = int32(poolCfg.MinConns)
	poolConfig.MaxConnLifetime = poolCfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = poolCfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = poolCfg.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
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
