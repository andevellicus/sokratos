package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/timeouts"
)

type metricsArgs struct {
	Report string `json:"report"`
	Window string `json:"window"`
}

// NewQueryMetrics returns a tool function that runs pre-built metrics queries.
// Also exported as QueryMetricsReport for direct use by slash commands.
func NewQueryMetrics(pool *pgxpool.Pool) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var call struct {
			Arguments metricsArgs `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &call); err != nil {
			return "Invalid arguments: " + err.Error(), nil
		}
		return QueryMetricsReport(ctx, pool, call.Arguments.Report, call.Arguments.Window)
	}
}

// QueryMetricsReport runs a pre-built metrics report. Exported for use by
// platform-agnostic slash command handlers (Telegram /metrics, etc.).
func QueryMetricsReport(ctx context.Context, pool *pgxpool.Pool, report, window string) (string, error) {
	if pool == nil {
		return "No database configured", nil
	}
	if window == "" {
		window = "1h"
	}

	// Validate window format (PostgreSQL interval).
	validWindows := map[string]bool{
		"5m": true, "15m": true, "30m": true,
		"1h": true, "2h": true, "6h": true, "12h": true,
		"24h": true, "1d": true, "7d": true, "30d": true,
	}
	if !validWindows[window] {
		return fmt.Sprintf("Invalid window %q. Use: 5m, 15m, 30m, 1h, 2h, 6h, 12h, 24h, 7d, 30d", window), nil
	}

	// Convert shorthand to PostgreSQL interval.
	interval := strings.Replace(window, "m", " minutes", 1)
	interval = strings.Replace(interval, "h", " hours", 1)
	interval = strings.Replace(interval, "d", " days", 1)

	qCtx, cancel := context.WithTimeout(ctx, timeouts.DBQuery)
	defer cancel()

	switch report {
	case "", "overview":
		return runOverview(qCtx, pool, interval, window)
	case "slots":
		return runSlots(qCtx, pool, interval, window)
	case "dispatch":
		return runDispatch(qCtx, pool, interval, window)
	case "latency":
		return runLatency(qCtx, pool, interval, window)
	case "tools":
		return runTools(qCtx, pool, interval, window)
	case "routines":
		return runRoutines(qCtx, pool, interval, window)
	default:
		return fmt.Sprintf("Unknown report %q. Available: overview, slots, dispatch, latency, tools, routines", report), nil
	}
}

func runOverview(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT name, COUNT(*), ROUND(AVG(value)::numeric, 1) AS avg_val
		 FROM metrics WHERE ts >= now() - $1::interval
		 GROUP BY name ORDER BY 2 DESC`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Metrics Overview (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var name string
		var count int64
		var avg float64
		if err := rows.Scan(&name, &count, &avg); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%-22s  %5d  avg=%.1f\n", name, count, avg)
		any = true
	}
	if !any {
		sb.WriteString("No metrics recorded yet.\n")
	}
	return sb.String(), nil
}

func runSlots(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT dims->>'backend' AS backend,
		        dims->>'strategy' AS strategy,
		        dims->>'result' AS result,
		        COUNT(*),
		        ROUND(AVG(value)::numeric) AS avg_ms,
		        ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY value)::numeric) AS p95_ms
		 FROM metrics WHERE name = 'slot.acquire' AND ts >= now() - $1::interval
		 GROUP BY 1, 2, 3 ORDER BY 4 DESC`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Slot Contention (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var backend, strategy, result string
		var count int64
		var avg, p95 float64
		if err := rows.Scan(&backend, &strategy, &result, &count, &avg, &p95); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s/%s → %s: %d calls, avg=%0.fms, p95=%0.fms\n",
			backend, strategy, result, count, avg, p95)
		any = true
	}
	if !any {
		sb.WriteString("No slot acquisitions recorded.\n")
	}
	return sb.String(), nil
}

func runDispatch(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT dims->>'result' AS result,
		        COALESCE(dims->>'phase', '-') AS phase,
		        COUNT(*),
		        ROUND(100.0 * COUNT(*) / SUM(COUNT(*)) OVER (), 1) AS pct
		 FROM metrics WHERE name = 'dispatch.decision' AND ts >= now() - $1::interval
		 GROUP BY 1, 2 ORDER BY 3 DESC`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Dispatch Effectiveness (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var result, phase string
		var count int64
		var pct float64
		if err := rows.Scan(&result, &phase, &count, &pct); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s (%s): %d (%.1f%%)\n", result, phase, count, pct)
		any = true
	}
	if !any {
		sb.WriteString("No dispatch decisions recorded.\n")
	}

	// Also show intercepts.
	var intercepts int64
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM metrics WHERE name = 'dispatch.intercept' AND ts >= now() - $1::interval`,
		interval).Scan(&intercepts)
	if intercepts > 0 {
		fmt.Fprintf(&sb, "\nIntercepts (neverDispatch): %d\n", intercepts)
	}

	return sb.String(), nil
}

func runLatency(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT dims->>'path' AS path, COUNT(*),
		        ROUND(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY value)::numeric) AS p50,
		        ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY value)::numeric) AS p95,
		        ROUND(MAX(value)::numeric) AS max_ms
		 FROM metrics WHERE name = 'message.total' AND ts >= now() - $1::interval
		 GROUP BY 1 ORDER BY 3 DESC`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Message Latency (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var path string
		var count int64
		var p50, p95, maxMs float64
		if err := rows.Scan(&path, &count, &p50, &p95, &maxMs); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s: %d msgs, p50=%0.fms, p95=%0.fms, max=%0.fms\n",
			path, count, p50, p95, maxMs)
		any = true
	}
	if !any {
		sb.WriteString("No message latency recorded.\n")
	}
	return sb.String(), nil
}

func runTools(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT dims->>'tool' AS tool, COUNT(*),
		        ROUND(AVG(value)::numeric) AS avg_ms,
		        ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY value)::numeric) AS p95_ms,
		        SUM(CASE WHEN dims->>'result' != 'ok' THEN 1 ELSE 0 END) AS errors
		 FROM metrics WHERE name = 'tool.exec' AND ts >= now() - $1::interval
		 GROUP BY 1 ORDER BY 3 DESC LIMIT 15`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Top Tools by Latency (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var tool string
		var count, errors int64
		var avg, p95 float64
		if err := rows.Scan(&tool, &count, &avg, &p95, &errors); err != nil {
			continue
		}
		errStr := ""
		if errors > 0 {
			errStr = fmt.Sprintf(" (%d err)", errors)
		}
		fmt.Fprintf(&sb, "%-20s %4d calls, avg=%0.fms, p95=%0.fms%s\n",
			tool, count, avg, p95, errStr)
		any = true
	}
	if !any {
		sb.WriteString("No tool executions recorded.\n")
	}
	return sb.String(), nil
}

func runRoutines(ctx context.Context, pool *pgxpool.Pool, interval, window string) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT dims->>'name' AS name,
		        dims->>'result' AS result,
		        COUNT(*),
		        ROUND(AVG(value)::numeric) AS avg_ms
		 FROM metrics WHERE name = 'routine.exec' AND ts >= now() - $1::interval
		 GROUP BY 1, 2 ORDER BY 1, 3 DESC`,
		interval)
	if err != nil {
		return "Query failed: " + err.Error(), nil
	}
	defer rows.Close()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Routine Executions (last %s)\n", window)
	sb.WriteString("─────────────────────────────\n")
	any := false
	for rows.Next() {
		var name, result string
		var count int64
		var avg float64
		if err := rows.Scan(&name, &result, &count, &avg); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%-20s %s: %d runs, avg=%0.fms\n", name, result, count, avg)
		any = true
	}
	if !any {
		sb.WriteString("No routine executions recorded.\n")
	}
	return sb.String(), nil
}
