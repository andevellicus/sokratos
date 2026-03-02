package tools

import (
	"context"
	"fmt"
	"strings"

	"sokratos/prompts"
)

// EnqueueConversationTriage queues a failed conversation triage for retry.
func EnqueueConversationTriage(q *RetryQueue, cfg TriageConfig, triageInput, exchange string, toolsUsed bool) {
	q.Enqueue(fmt.Sprintf("conversation triage: %.60s", triageInput), func() error {
		return retryConversationTriage(cfg, triageInput, exchange, toolsUsed)
	})
}

// EnqueueEmailTriage queues a failed email triage for retry.
func EnqueueEmailTriage(q *RetryQueue, cfg TriageConfig, triageInput, formatted string) {
	q.Enqueue(fmt.Sprintf("email triage: %.60s", triageInput), func() error {
		return retryEmailTriage(cfg, triageInput, formatted)
	})
}

func retryConversationTriage(cfg TriageConfig, triageInput, exchange string, toolsUsed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
	defer cancel()

	threshold := float64(3)
	if !toolsUsed {
		threshold = 5
	}

	return triageAndSave(ctx, cfg, TriageSaveRequest{
		TriagePrompt:  strings.TrimSpace(prompts.ConversationTriage),
		TriageInput:   triageInput,
		SourceContent: exchange,
		SourceLabel:   "Source exchange",
		DomainTag:     "conversation",
		MemoryType:    "general",
		Source:        "conversation",
		MaxTriageLen:  8000,
		ShouldSave: func(r *triageResult) bool {
			return r.SalienceScore >= threshold
		},
	})
}

func retryEmailTriage(cfg TriageConfig, triageInput, formatted string) error {
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutConversationTriage)
	defer cancel()

	return triageAndSave(ctx, cfg, TriageSaveRequest{
		TriagePrompt:  strings.TrimSpace(prompts.EmailTriage),
		TriageInput:   triageInput,
		SourceContent: formatted,
		SourceLabel:   "Source email",
		DomainTag:     "email",
		MemoryType:    "email",
		Source:        "email",
		MaxTriageLen:  8000,
		ShouldSave: func(r *triageResult) bool {
			if r.Save != nil && !*r.Save {
				return false
			}
			return r.SalienceScore >= 1
		},
	})
}
