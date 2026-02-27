package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

type askDBArgs struct {
	Query string `json:"natural_language_query"`
}

type text2sqlRequest struct {
	Model     string        `json:"model"`
	Messages  []text2sqlMsg `json:"messages"`
	Stream    bool          `json:"stream"`
	KeepAlive string        `json:"keep_alive,omitempty"` // auto-unload after this duration (e.g. "30s")
}

type text2sqlMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type text2sqlResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// NewAskDatabase returns a ToolFunc that translates a natural language query
// into PostgreSQL via the Arctic-Text2SQL model, executes it, and returns the
// results. The database schema is fetched dynamically from information_schema
// so the SQL model always sees the current table definitions.
func NewAskDatabase(pool *pgxpool.Pool, text2sqlURL string) ToolFunc {
	httpClient := &http.Client{Timeout: TimeoutText2SQL}

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

		reqBody := text2sqlRequest{
			Model: "Arctic-Text2SQL-R1-7B.Q8_0",
			Messages: []text2sqlMsg{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: a.Query},
			},
			Stream:    false,
			KeepAlive: "30s",
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Sprintf("failed to marshal request: %v", err), nil
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, text2sqlURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return fmt.Sprintf("failed to create request: %v", err), nil
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return fmt.Sprintf("Text2SQL model request failed (is the model loaded?): %v", err), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Sprintf("Text2SQL model returned status %d: %s", resp.StatusCode, string(respBody)), nil
		}

		var t2sResp text2sqlResponse
		if err := json.NewDecoder(resp.Body).Decode(&t2sResp); err != nil {
			return fmt.Sprintf("failed to decode Text2SQL response: %v", err), nil
		}

		if len(t2sResp.Choices) == 0 {
			return "Text2SQL model returned no output.", nil
		}

		sql := strings.TrimSpace(t2sResp.Choices[0].Message.Content)
		sql = stripSQLFences(sql)

		if sql == "" {
			return "Text2SQL model returned empty SQL.", nil
		}

		// Validate that the output actually looks like SQL (not prose).
		if !looksLikeSQL(sql) {
			preview := sql
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			return fmt.Sprintf("Text2SQL model returned non-SQL output: %s", preview), nil
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
// Used to catch the failure mode where the Text2SQL model generates prose instead of SQL.
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
				s := fmt.Sprintf("%v", v)
				if len(s) > 100 {
					s = s[:100] + "..."
				}
				parts[i] = s
			}
		}
		b.WriteString(strings.Join(parts, " | "))
		b.WriteString("\n")
		rowCount++
		if rowCount >= 50 {
			b.WriteString("... (truncated at 50 rows)\n")
			break
		}
	}

	if rowCount == 0 {
		b.WriteString("(no rows)\n")
	}

	return b.String(), nil
}

