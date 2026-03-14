package toolreg

import (
	"strings"
	"sync"

	"sokratos/prompts"
)

// MaxDelegateContextLen is the max characters for context in delegate calls.
const MaxDelegateContextLen = 12000

// DelegateSystemPrompt is the system prompt for delegate subagent calls.
var DelegateSystemPrompt = strings.TrimSpace(prompts.DelegateSystem)

// DelegateConfig holds the mutable grammar and allowed-tools list for
// delegate_task. Updated atomically when skills are created or deleted.
type DelegateConfig struct {
	mu      sync.RWMutex
	grammar string
	allowed map[string]bool
}

// NewDelegateConfig creates a config with the given tool names and grammar.
func NewDelegateConfig(tools []string, grammar string) *DelegateConfig {
	allowed := make(map[string]bool, len(tools))
	for _, name := range tools {
		allowed[name] = true
	}
	return &DelegateConfig{grammar: grammar, allowed: allowed}
}

// Update replaces the grammar and allowed-tools list atomically.
func (dc *DelegateConfig) Update(tools []string, grammar string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.grammar = grammar
	dc.allowed = make(map[string]bool, len(tools))
	for _, name := range tools {
		dc.allowed[name] = true
	}
}

// Grammar returns the current GBNF grammar.
func (dc *DelegateConfig) Grammar() string {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.grammar
}

// IsAllowed returns whether the named tool is delegatable.
func (dc *DelegateConfig) IsAllowed(name string) bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.allowed[name]
}
