package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/engine"
)

// NewReplyToJob returns a ToolFunc that delivers a user message to a
// background Brain job's input channel.
func NewReplyToJob(stateMgr *engine.StateManager) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			JobID   string `json:"job_id"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.JobID == "" || a.Message == "" {
			return "Both job_id and message are required.", nil
		}

		job := stateMgr.GetJob(a.JobID)
		if job == nil {
			return fmt.Sprintf("No active background job with ID %q.", a.JobID), nil
		}

		select {
		case job.InputCh <- a.Message:
			return "Message delivered to background job.", nil
		default:
			return "The background job hasn't processed the previous message yet. Try again shortly.", nil
		}
	}
}

// NewCancelJob returns a ToolFunc that cancels a background Brain job.
func NewCancelJob(stateMgr *engine.StateManager) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.JobID == "" {
			return "job_id is required.", nil
		}

		job := stateMgr.GetJob(a.JobID)
		if job == nil {
			return fmt.Sprintf("No active background job with ID %q.", a.JobID), nil
		}

		job.Cancel()
		return "Job cancelled.", nil
	}
}
