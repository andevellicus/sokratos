package platform

import (
	"context"
	"strconv"
	"time"
)

// IncomingMessage is the platform-agnostic representation of a user message.
type IncomingMessage struct {
	ID        string // platform message ID (Telegram: strconv.Itoa(msg.MessageID))
	ChannelID string // where to reply (Telegram: strconv.FormatInt(chatID, 10))
	Text      string // message text or caption
	SenderTag string // human-readable sender (e.g. "@username")
	SenderID  string // for auth checks
	PhotoData []byte // raw image bytes, nil if no photo
	PhotoMIME string // MIME type of photo
}

// PipelineID returns message ID as int64 for memory isolation.
// Returns 0 for non-numeric IDs.
func (m *IncomingMessage) PipelineID() int64 {
	n, _ := strconv.ParseInt(m.ID, 10, 64)
	return n
}

// Sender handles all outbound messaging.
type Sender interface {
	// Send sends markdown-formatted text, optionally as reply. Returns sent message ID.
	Send(ctx context.Context, channelID, markdown, replyTo string) (string, error)
	// Edit edits an existing message.
	Edit(ctx context.Context, channelID, messageID, markdown string) error
	// StartTyping begins a typing indicator. Returns cancel func.
	StartTyping(ctx context.Context, channelID string) context.CancelFunc
	// Broadcast sends to all configured recipients.
	Broadcast(ctx context.Context, markdown string)
}

// Confirmer handles user confirmation for gated tool calls.
type Confirmer interface {
	// Confirm shows a confirmation prompt, blocks until approve/deny/timeout.
	Confirm(ctx context.Context, channelID, description string, timeout time.Duration) (approved bool, err error)
}

// Receiver provides incoming messages.
type Receiver interface {
	Messages() <-chan *IncomingMessage
	// ReadReply blocks until the next text message arrives on the channel.
	ReadReply() (string, error)
}

// CommandRegistrar registers platform slash commands.
type CommandRegistrar interface {
	RegisterCommands(cmds []Command) error
}

// Command describes a slash command.
type Command struct {
	Name        string
	Description string
}

// ProgressHandle tracks a single editable progress message. It uses
// Send to create the initial message and Edit to update it in place.
type ProgressHandle struct {
	ChannelID string
	MessageID string
	Sender    Sender
}

// NewProgressHandle sends an initial progress message and returns a handle
// that can be updated in place via Edit.
func NewProgressHandle(ctx context.Context, sender Sender, channelID, initial, replyTo string) (*ProgressHandle, error) {
	msgID, err := sender.Send(ctx, channelID, initial, replyTo)
	if err != nil {
		return nil, err
	}
	return &ProgressHandle{
		ChannelID: channelID,
		MessageID: msgID,
		Sender:    sender,
	}, nil
}

// Update edits the progress message in place.
func (h *ProgressHandle) Update(ctx context.Context, markdown string) error {
	return h.Sender.Edit(ctx, h.ChannelID, h.MessageID, markdown)
}

// MenuOption represents a single selectable option in a structured menu.
type MenuOption struct {
	Label string // display text
	Value string // returned on selection
}

// MenuPrompter presents interactive menus with clickable options.
// Platforms that support this should implement it as an optional interface
// checked via type assertion.
type MenuPrompter interface {
	// PromptWithOptions shows a prompt with clickable options.
	// Returns selected index, or -1 on timeout/cancel.
	PromptWithOptions(ctx context.Context, channelID, prompt string, options []MenuOption, timeout time.Duration) (int, error)
}

// Platform bundles all platform capabilities.
type Platform interface {
	Sender
	Confirmer
	Receiver
	CommandRegistrar
}
