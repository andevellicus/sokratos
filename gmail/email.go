package gmail

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	gm "google.golang.org/api/gmail/v1"
)

// Email holds parsed fields from a Gmail message.
type Email struct {
	ID      string
	From    string
	To      string
	CC      string
	BCC     string
	Subject string
	Body    string
	Snippet string
	Date    time.Time
}

// ParseMessage extracts an Email from a raw Gmail API message.
func ParseMessage(msg *gm.Message) Email {
	e := Email{
		ID:      msg.Id,
		Snippet: msg.Snippet,
	}

	for _, h := range msg.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			e.From = h.Value
		case "to":
			e.To = h.Value
		case "cc":
			e.CC = h.Value
		case "bcc":
			e.BCC = h.Value
		case "subject":
			e.Subject = h.Value
		case "date":
			if t, err := time.Parse(time.RFC1123Z, h.Value); err == nil {
				e.Date = t
			} else if t, err := time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", h.Value); err == nil {
				e.Date = t
			}
		}
	}

	e.Body = extractPlaintext(msg.Payload)
	return e
}

// FetchEmails lists messages matching query and fetches full details for each.
func FetchEmails(svc *gm.Service, query string, maxResults int64) ([]Email, error) {
	if maxResults <= 0 {
		maxResults = 10
	}

	list, err := svc.Users.Messages.List("me").Q(query).MaxResults(maxResults).Do()
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	var emails []Email
	for _, m := range list.Messages {
		msg, err := svc.Users.Messages.Get("me", m.Id).Format("full").Do()
		if err != nil {
			continue
		}

		emails = append(emails, ParseMessage(msg))
	}

	return emails, nil
}

// FormatForEmbedding produces a full-text block for vector embedding that
// includes headers, body (truncated to ~1200 chars), and triage results.
func FormatForEmbedding(e Email, triageSummary string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\n", e.From)
	fmt.Fprintf(&b, "To: %s\n", e.To)
	if e.CC != "" {
		fmt.Fprintf(&b, "CC: %s\n", e.CC)
	}
	if e.BCC != "" {
		fmt.Fprintf(&b, "BCC: %s\n", e.BCC)
	}
	fmt.Fprintf(&b, "Subject: %s\n", e.Subject)
	if !e.Date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\n", e.Date.Format("2006-01-02"))
	}

	body := e.Body
	if body == "" {
		body = e.Snippet
	}
	if len(body) > 1200 {
		body = body[:1200] + "..."
	}
	fmt.Fprintf(&b, "\n%s\n", body)

	if triageSummary != "" {
		fmt.Fprintf(&b, "\n%s", triageSummary)
	}

	return b.String()
}

// FormatEmailSummary returns a human-readable summary of an email,
// with the body truncated at 500 characters.
func FormatEmailSummary(e Email) string {
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	if body == "" {
		body = e.Snippet
	}

	dateStr := ""
	if !e.Date.IsZero() {
		dateStr = e.Date.Format("2006-01-02 15:04")
	}

	to := e.To
	if to == "" {
		to = "(unknown)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\nTo: %s\n", e.From, to)
	if e.CC != "" {
		fmt.Fprintf(&b, "CC: %s\n", e.CC)
	}
	if e.BCC != "" {
		fmt.Fprintf(&b, "BCC: %s\n", e.BCC)
	}
	fmt.Fprintf(&b, "Subject: %s\nDate: %s\n\n%s", e.Subject, dateStr, body)

	return b.String()
}

// SendEmail sends a plain-text email via the Gmail API.
func SendEmail(svc *gm.Service, to, subject, body string) error {
	raw := fmt.Sprintf("To: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s", to, subject, body)
	msg := &gm.Message{
		Raw: base64.URLEncoding.EncodeToString([]byte(raw)),
	}
	_, err := svc.Users.Messages.Send("me", msg).Do()
	return err
}

// extractPlaintext walks the MIME structure recursively, preferring text/plain.
func extractPlaintext(part *gm.MessagePart) string {
	if part == nil {
		return ""
	}

	if part.MimeType == "text/plain" && part.Body != nil && part.Body.Data != "" {
		data, err := base64.URLEncoding.DecodeString(part.Body.Data)
		if err != nil {
			return ""
		}
		return string(data)
	}

	for _, child := range part.Parts {
		if text := extractPlaintext(child); text != "" {
			return text
		}
	}

	return ""
}
