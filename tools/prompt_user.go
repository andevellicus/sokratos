package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sokratos/platform"
)

const maxPromptOptions = 8

type promptUserArgs struct {
	Prompt  string   `json:"prompt"`
	Options []string `json:"options"`
}

// NewPromptUser returns a ToolFunc that presents a menu of options to the user
// and returns their selection. If the platform supports MenuPrompter, inline
// buttons are used; otherwise, falls back to a numbered list with text reply.
func NewPromptUser(plat platform.Platform, channelIDFn func() string, timeout time.Duration) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a, err := ParseArgs[promptUserArgs](args)
		if err != nil {
			return err.Error(), nil
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return "prompt is required", nil
		}
		if len(a.Options) < 2 {
			return "at least 2 options are required", nil
		}
		if len(a.Options) > maxPromptOptions {
			return fmt.Sprintf("too many options (%d), max is %d", len(a.Options), maxPromptOptions), nil
		}

		channelID := channelIDFn()
		if channelID == "" {
			return "no active channel", nil
		}

		// Try MenuPrompter (inline keyboard buttons).
		if mp, ok := plat.(platform.MenuPrompter); ok {
			options := make([]platform.MenuOption, len(a.Options))
			for i, label := range a.Options {
				options[i] = platform.MenuOption{Label: label, Value: label}
			}
			idx, err := mp.PromptWithOptions(ctx, channelID, a.Prompt, options, timeout)
			if err != nil {
				return fmt.Sprintf("prompt cancelled: %v", err), nil
			}
			if idx < 0 {
				return "User did not select an option (timed out).", nil
			}
			return a.Options[idx], nil
		}

		// Fallback: numbered list via Send + ReadReply.
		var sb strings.Builder
		sb.WriteString(a.Prompt)
		sb.WriteString("\n\n")
		for i, opt := range a.Options {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, opt)
		}
		sb.WriteString("\nReply with the number of your choice.")
		plat.Send(ctx, channelID, sb.String(), "")

		reply, err := plat.ReadReply()
		if err != nil {
			return fmt.Sprintf("failed to read reply: %v", err), nil
		}

		// Parse number.
		reply = strings.TrimSpace(reply)
		var idx int
		if _, err := fmt.Sscanf(reply, "%d", &idx); err != nil || idx < 1 || idx > len(a.Options) {
			return fmt.Sprintf("Invalid selection: %q. Expected a number between 1 and %d.", reply, len(a.Options)), nil
		}
		return a.Options[idx-1], nil
	}
}
