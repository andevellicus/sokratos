package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/prompts"
)

const maxSubagentDataLen = 12000

type dispatchSubagentArgs struct {
	TaskDirective string `json:"task_directive"`
	RawData       string `json:"raw_data"`
}

var subagentSystemPrompt = strings.TrimSpace(prompts.Subagent)

// NewDispatchSubagent returns a ToolFunc that delegates a data-processing task
// to the lightweight subagent. Follows soft error convention.
func NewDispatchSubagent(sc *SubagentClient) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a dispatchSubagentArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(a.TaskDirective) == "" {
			return "task_directive is required", nil
		}
		if strings.TrimSpace(a.RawData) == "" {
			return "raw_data is required", nil
		}

		data := a.RawData
		if len(data) > maxSubagentDataLen {
			data = data[:maxSubagentDataLen] + "\n... (truncated)"
		}

		userContent := fmt.Sprintf("## Task\n%s\n\n## Data\n%s", a.TaskDirective, data)

		result, err := sc.Complete(ctx, subagentSystemPrompt, userContent, 2048)
		if err != nil {
			return fmt.Sprintf("subagent unavailable: %v", err), nil
		}

		return result, nil
	}
}
