package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/engine"
)

type updateStateArgs struct {
	Status string `json:"status"`
	Task   string `json:"task"`
}

// NewUpdateState returns a ToolFunc that updates the agent state via the given
// StateManager. The LLM can pass a new status and current task description.
func NewUpdateState(sm *engine.StateManager) ToolFunc {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[updateStateArgs](args)
		if err != nil {
			return err.Error(), nil
		}

		sm.Update(func(s *engine.AgentState) {
			if a.Status != "" {
				s.Status = a.Status
			}
			if a.Task != "" {
				s.CurrentTask = a.Task
			}
			s.StepCount++
		})

		state := sm.GetState()
		return fmt.Sprintf("State updated: status=%s, task=%s, step=%d", state.Status, state.CurrentTask, state.StepCount), nil
	}
}
