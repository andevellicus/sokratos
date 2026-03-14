package main

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/clients"
	"sokratos/google"
	"sokratos/pipelines"
	"sokratos/tools"
)

// registerGmailTools registers tools for searching and interacting with Gmail.
func registerGmailTools(registry *tools.Registry, pool *pgxpool.Pool, triageCfg *pipelines.TriageConfig, emailDisplayBatch int, sc *clients.SubagentClient) {
	if google.GmailService == nil {
		return
	}
	registry.Register("search_email", tools.NewSearchEmail(google.GmailService, pool, triageCfg, emailDisplayBatch, sc), tools.ToolSchema{
		Name:          "search_email",
		Description:   "Search Gmail inbox with optional time bounds",
		ProgressLabel: "Checking email...",
		Params: []tools.ParamSchema{
			{Name: "query", Type: "string", Required: false},
			{Name: "time_min", Type: "string", Required: false},
			{Name: "time_max", Type: "string", Required: false},
			{Name: "max_results", Type: "number", Required: false},
		},
	})
	registry.Register("send_email", tools.NewSendEmail(google.GmailService), tools.ToolSchema{
		Name:          "send_email",
		Description:   "Send a plain-text email",
		ProgressLabel: "Sending email...",
		Params: []tools.ParamSchema{
			{Name: "to", Type: "string", Required: true},
			{Name: "subject", Type: "string", Required: true},
			{Name: "body", Type: "string", Required: true},
		},
		ConfirmFormat: func(args json.RawMessage) string {
			var a struct {
				To      string `json:"to"`
				Subject string `json:"subject"`
			}
			_ = json.Unmarshal(args, &a)
			return fmt.Sprintf("⚠️ Send email to %s\nSubject: %q", a.To, a.Subject)
		},
		ConfirmCacheKey: func(args json.RawMessage) string {
			var a struct {
				To      string `json:"to"`
				Subject string `json:"subject"`
			}
			if json.Unmarshal(args, &a) == nil {
				return "send_email:" + a.To + ":" + a.Subject
			}
			return "send_email"
		},
	})
}
