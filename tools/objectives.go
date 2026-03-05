package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/objectives"
)

// NewManageObjectives returns a ToolFunc for creating, updating, and managing objectives.
func NewManageObjectives(db *pgxpool.Pool) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Op          string `json:"op"`
			Summary     string `json:"summary"`
			ObjectiveID int64  `json:"objective_id"`
			Priority    string `json:"priority"`
			Notes       string `json:"notes"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}

		switch a.Op {
		case "add":
			if a.Summary == "" {
				return "summary is required for add", nil
			}
			// Dedup check.
			similar, _ := objectives.FindSimilar(ctx, db, a.Summary)
			if len(similar) > 0 {
				return fmt.Sprintf("Similar objective already exists: #%d %s", similar[0].ID, similar[0].Summary), nil
			}
			id, err := objectives.Create(ctx, db, a.Summary, a.Priority, "explicit")
			if err != nil {
				return fmt.Sprintf("failed to create objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d created: %s", id, a.Summary), nil

		case "update":
			if a.ObjectiveID <= 0 {
				return "objective_id is required for update", nil
			}
			if a.Notes != "" {
				if err := objectives.AppendProgress(ctx, db, a.ObjectiveID, a.Notes); err != nil {
					return fmt.Sprintf("failed to append progress: %v", err), nil
				}
			}
			if a.Priority != "" {
				if err := objectives.UpdatePriority(ctx, db, a.ObjectiveID, a.Priority); err != nil {
					return fmt.Sprintf("failed to update priority: %v", err), nil
				}
			}
			g, err := objectives.Get(ctx, db, a.ObjectiveID)
			if err != nil {
				return fmt.Sprintf("failed to get objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d updated.\n%s", a.ObjectiveID, objectives.FormatList([]objectives.Objective{*g})), nil

		case "pause":
			if a.ObjectiveID <= 0 {
				return "objective_id is required for pause", nil
			}
			if err := objectives.UpdateStatus(ctx, db, a.ObjectiveID, "paused"); err != nil {
				return fmt.Sprintf("failed to pause objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d paused.", a.ObjectiveID), nil

		case "resume":
			if a.ObjectiveID <= 0 {
				return "objective_id is required for resume", nil
			}
			if err := objectives.UpdateStatus(ctx, db, a.ObjectiveID, "active"); err != nil {
				return fmt.Sprintf("failed to resume objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d resumed.", a.ObjectiveID), nil

		case "complete":
			if a.ObjectiveID <= 0 {
				return "objective_id is required for complete", nil
			}
			if err := objectives.Complete(ctx, db, a.ObjectiveID); err != nil {
				return fmt.Sprintf("failed to complete objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d completed.", a.ObjectiveID), nil

		case "retire":
			if a.ObjectiveID <= 0 {
				return "objective_id is required for retire", nil
			}
			if err := objectives.Retire(ctx, db, a.ObjectiveID); err != nil {
				return fmt.Sprintf("failed to retire objective: %v", err), nil
			}
			return fmt.Sprintf("Objective #%d retired.", a.ObjectiveID), nil

		case "list":
			list, err := objectives.ListAll(ctx, db)
			if err != nil {
				return fmt.Sprintf("failed to list objectives: %v", err), nil
			}
			return objectives.FormatList(list), nil

		default:
			return fmt.Sprintf("unknown op %q (use add, update, pause, resume, complete, retire, list)", a.Op), nil
		}
	}
}
