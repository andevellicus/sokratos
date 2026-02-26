package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/gmail"
	"sokratos/logger"

	gm "google.golang.org/api/gmail/v1"
)

type sendEmailArgs struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// NewSendEmail returns a ToolFunc that sends a plain-text email via Gmail.
func NewSendEmail(svc *gm.Service) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a sendEmailArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v", err), nil
		}
		if a.To == "" {
			return "error: 'to' is required", nil
		}
		if a.Subject == "" {
			return "error: 'subject' is required", nil
		}
		if a.Body == "" {
			return "error: 'body' is required", nil
		}

		if err := gmail.SendEmail(svc, a.To, a.Subject, a.Body); err != nil {
			logger.Log.Errorf("[send_email] failed: %v", err)
			return fmt.Sprintf("Failed to send email: %v", err), nil
		}

		logger.Log.Infof("[send_email] sent to=%s subject=%q", a.To, a.Subject)
		return fmt.Sprintf("Email sent successfully to %s.", a.To), nil
	}
}
