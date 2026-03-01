package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// NewRunCode returns a tool that executes ad-hoc JavaScript in the goja sandbox.
// Reuses the same runtime as skills: ES5, http_request bridge, 30s timeout.
func NewRunCode() ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.Code == "" {
			return "code is required", nil
		}

		if err := ValidateSkillSource(a.Code); err != nil {
			return fmt.Sprintf("syntax error: %v", err), nil
		}

		return ExecuteSkill(ctx, "run_code", a.Code, "", nil)
	}
}
