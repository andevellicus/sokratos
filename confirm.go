package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sokratos/logger"
	"sokratos/platform"
	"sokratos/tools"
)

// confirmToolExec wraps a registry's Execute with platform-based confirmation
// for externally-visible actions. Confirmation metadata (prompt format, cache
// key) is looked up from each tool's ToolSchema, so new confirmable tools
// just need to set ConfirmFormat/ConfirmCacheKey at registration time.
func confirmToolExec(
	registry *tools.Registry,
	confirmer platform.Confirmer,
	confirmTools map[string]bool,
	confirmTimeout time.Duration,
) func(context.Context, json.RawMessage) (string, error) {
	cache := tools.NewApprovalCache(5 * time.Minute)

	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &call); err == nil && confirmTools[call.Name] {
			// Build cache key from schema metadata, falling back to tool name.
			cacheKey := call.Name
			if schema, ok := registry.SchemaFor(call.Name); ok && schema.ConfirmCacheKey != nil {
				if k := schema.ConfirmCacheKey(call.Arguments); k != "" {
					cacheKey = k
				}
			}

			if cache.Check(cacheKey) {
				logger.Log.Debugf("[confirm] auto-approved %s (cached)", cacheKey)
				return registry.Execute(ctx, raw)
			}

			// Build confirmation prompt from schema metadata, falling back to generic.
			desc := fmt.Sprintf("⚠️ Execute %s?", call.Name)
			if schema, ok := registry.SchemaFor(call.Name); ok && schema.ConfirmFormat != nil {
				desc = schema.ConfirmFormat(call.Arguments)
			}

			approved, err := confirmer.Confirm(ctx, "", desc, confirmTimeout)
			if err != nil {
				return "Action cancelled — confirmation error.", nil
			}
			if approved {
				cache.Record(cacheKey)
				return registry.Execute(ctx, raw)
			}
			return "Action cancelled by user.", nil
		}
		return registry.Execute(ctx, raw)
	}
}
