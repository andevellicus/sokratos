package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sokratos/google"
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
			return "", Errorf("invalid arguments: %v", err)
		}
		if a.To == "" {
			return "", Errorf("error: 'to' is required")
		}
		if a.Subject == "" {
			return "", Errorf("error: 'subject' is required")
		}
		if a.Body == "" {
			return "", Errorf("error: 'body' is required")
		}

		if err := google.SendEmail(svc, a.To, a.Subject, a.Body); err != nil {
			if google.IsAuthError(err) {
				return "", Errorf("%s", google.AuthErrorMessage)
			}
			logger.Log.Errorf("[send_email] failed: %v", err)
			return "", Errorf("Failed to send email: %v", err)
		}

		logger.Log.Infof("[send_email] sent to=%s subject=%q", a.To, a.Subject)
		return fmt.Sprintf("Email sent successfully to %s.", a.To), nil
	}
}
