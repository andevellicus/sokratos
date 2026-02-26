package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/engine"
)

type setPreferenceArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NewSetPreference returns a ToolFunc that sets a user preference in the
// AgentState, persisted to the user_preferences table in PostgreSQL.
// Common keys: name, location, timezone, language.
func NewSetPreference(sm *engine.StateManager) ToolFunc {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var a setPreferenceArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		a.Key = strings.TrimSpace(a.Key)
		a.Value = strings.TrimSpace(a.Value)
		if a.Key == "" {
			return "error: key is required", nil
		}

		if a.Value == "" {
			if err := sm.DeletePref(a.Key); err != nil {
				return fmt.Sprintf("failed to delete preference: %v", err), nil
			}
			return fmt.Sprintf("Preference %q removed.", a.Key), nil
		}

		if err := sm.SetPref(a.Key, a.Value); err != nil {
			return fmt.Sprintf("failed to save preference: %v", err), nil
		}
		return fmt.Sprintf("Preference saved: %s = %s", a.Key, a.Value), nil
	}
}
