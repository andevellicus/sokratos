package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/logger"
	"sokratos/textutil"
)

type askDBArgs struct {
	Query string `json:"natural_language_query"`
}

// NewAskDatabase returns a ToolFunc that translates a natural language query
// into PostgreSQL via the subagent, executes it, and returns the results.
// The database schema is fetched dynamically from information_schema so the
// model always sees the current table definitions.
func NewAskDatabase(pool *pgxpool.Pool, sc *clients.SubagentClient) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a askDBArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Query == "" {
			return "error: natural_language_query is required", nil
		}

		// Fetch the live schema from the database.
		schemaDDL, err := fetchSchemaDDL(ctx, pool)
		if err != nil {
			return fmt.Sprintf("failed to fetch database schema: %v", err), nil
		}

		systemPrompt := "You are a PostgreSQL SQL generator. Given a natural language query, output ONLY valid PostgreSQL SQL. " +
			"No explanations, no markdown, no code fences — just the raw SQL statement.\n\n" +
			"The database has these tables:\n\n" + schemaDDL +
			"\n\nOutput ONLY the SQL statement. Do not include any explanations or formatting."

		result, err := sc.Complete(ctx, systemPrompt, a.Query, 512)
		if err != nil {
			return fmt.Sprintf("SQL generation failed: %v", err), nil
		}

		sql := strings.TrimSpace(result)
		sql = stripSQLFences(sql)

		if sql == "" {
			return "Subagent returned empty SQL.", nil
		}

		// Validate that the output actually looks like SQL (not prose).
		if !looksLikeSQL(sql) {
			preview := textutil.Truncate(sql, 200)
			return fmt.Sprintf("Subagent returned non-SQL output: %s", preview), nil
		}

		logger.Log.Infof("[ask_database] generated SQL: %s", sql)

		// Block all mutation and DDL statements — ask_database is read-only.
		upper := strings.ToUpper(strings.TrimSpace(sql))
		for _, prefix := range []string{"DROP ", "TRUNCATE ", "ALTER ", "CREATE ", "GRANT ", "REVOKE ", "INSERT ", "UPDATE ", "DELETE "} {
			if strings.HasPrefix(upper, prefix) {
				return fmt.Sprintf("Blocked: %sstatements are not allowed via ask_database (read-only).", prefix), nil
			}
		}

		return executeQuery(ctx, pool, sql)
	}
}

// fetchSchemaDDL queries information_schema for all user tables in the public
// schema and reconstructs a simplified CREATE TABLE DDL for each.
func fetchSchemaDDL(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	rows, err := pool.Query(ctx,
		`SELECT table_name, column_name, data_type, is_nullable, column_default
		 FROM information_schema.columns
		 WHERE table_schema = 'public'
		 ORDER BY table_name, ordinal_position`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type colInfo struct {
		name, dataType, nullable, defaultVal string
	}
	tables := make(map[string][]colInfo)
	var tableOrder []string
	seen := make(map[string]bool)

	for rows.Next() {
		var tbl, col, dt, nullable string
		var dflt *string
		if err := rows.Scan(&tbl, &col, &dt, &nullable, &dflt); err != nil {
			continue
		}
		if !seen[tbl] {
			seen[tbl] = true
			tableOrder = append(tableOrder, tbl)
		}
		ci := colInfo{name: col, dataType: dt, nullable: nullable}
		if dflt != nil {
			ci.defaultVal = *dflt
		}
		tables[tbl] = append(tables[tbl], ci)
	}

	var b strings.Builder
	for _, tbl := range tableOrder {
		fmt.Fprintf(&b, "CREATE TABLE %s (\n", tbl)
		cols := tables[tbl]
		for i, c := range cols {
			fmt.Fprintf(&b, "    %s %s", c.name, c.dataType)
			if c.nullable == "NO" {
				b.WriteString(" NOT NULL")
			}
			if c.defaultVal != "" {
				fmt.Fprintf(&b, " DEFAULT %s", c.defaultVal)
			}
			if i < len(cols)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(");\n\n")
	}

	return b.String(), nil
}

// looksLikeSQL returns true if the string starts with a recognized SQL keyword.
// Used to catch the failure mode where the model generates prose instead of SQL.
func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range []string{"SELECT ", "WITH ", "EXPLAIN "} {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}

func stripSQLFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func executeQuery(ctx context.Context, pool *pgxpool.Pool, sql string) (string, error) {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return fmt.Sprintf("SQL error: %v\nSQL: %s", err, sql), nil
	}
	defer rows.Close()

	cols := rows.FieldDescriptions()
	colNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = string(c.Name)
	}

	var b strings.Builder
	header := strings.Join(colNames, " | ")
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString(strings.Repeat("-", len(header)))
	b.WriteString("\n")

	rowCount := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			continue
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				parts[i] = "NULL"
			} else {
				s := textutil.Truncate(fmt.Sprintf("%v", v), 100)
				parts[i] = s
			}
		}
		b.WriteString(strings.Join(parts, " | "))
		b.WriteString("\n")
		rowCount++
		const maxQueryRows = 50
		if rowCount >= maxQueryRows {
			fmt.Fprintf(&b, "... (truncated at %d rows)\n", maxQueryRows)
			break
		}
	}

	if rowCount == 0 {
		b.WriteString("(no rows)\n")
	}

	return b.String(), nil
}
