package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/llm"
	"sokratos/logger"
)

// Task represents a row in the PostgreSQL tasks table.
type Task struct {
	ID          int64
	Description string
	DueAt       *time.Time
	Recurrence  time.Duration // stored as nanoseconds (BIGINT) in DB
	Status      string        // "pending", "completed"
}

// AgentState represents the in-memory state of the autonomous agent.
type AgentState struct {
	Status      string            `json:"status"`
	CurrentTask string            `json:"current_task"`
	StepCount   int               `json:"step_count"`
	UserPrefs   map[string]string `json:"user_prefs,omitempty"`
}

// ToMarkdown returns a formatted Markdown representation of the agent state.
func (s AgentState) ToMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Status:** %s\n", s.Status)
	fmt.Fprintf(&b, "**Current Task:** %s\n", s.CurrentTask)
	fmt.Fprintf(&b, "**Step Count:** %d\n", s.StepCount)
	if len(s.UserPrefs) > 0 {
		b.WriteString("**User Preferences:**\n")
		for k, v := range s.UserPrefs {
			fmt.Fprintf(&b, "- %s: %s\n", k, v)
		}
	}
	return b.String()
}

// StateManager provides thread-safe access to the agent state. Transient fields
// (status, current_task, step_count) live in memory only. User preferences are
// backed by the user_preferences table in PostgreSQL.
type StateManager struct {
	mu       sync.RWMutex
	state    AgentState
	pool     *pgxpool.Pool // nil when running without database
	messages []llm.Message // conversation context (not persisted)
}

// NewStateManager creates a StateManager. If pool is non-nil, it loads user
// preferences from the database. Otherwise it starts with an empty state.
func NewStateManager(pool *pgxpool.Pool) *StateManager {
	sm := &StateManager{
		pool: pool,
		state: AgentState{
			Status:    "idle",
			UserPrefs: make(map[string]string),
		},
	}
	if pool != nil {
		sm.loadPrefsFromDB()
	}
	return sm
}

// RefreshPrefs reloads all user preferences from the database into memory.
// Safe to call periodically (e.g., each heartbeat tick) to pick up externally
// added or modified preferences.
func (sm *StateManager) RefreshPrefs() {
	if sm.pool == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.loadPrefsFromDB()
}

// loadPrefsFromDB reads all user preferences from the database into memory.
// Caller must hold sm.mu.
func (sm *StateManager) loadPrefsFromDB() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := sm.pool.Query(ctx, `SELECT key, value FROM user_preferences`)
	if err != nil {
		logger.Log.Warnf("[state] failed to load preferences from DB: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		sm.state.UserPrefs[k] = v
	}
	if len(sm.state.UserPrefs) > 0 {
		logger.Log.Infof("[state] loaded %d user preferences from DB", len(sm.state.UserPrefs))
	}
}

// GetState returns a copy of the current agent state.
func (sm *StateManager) GetState() AgentState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	cp := sm.state
	if sm.state.UserPrefs != nil {
		cp.UserPrefs = make(map[string]string, len(sm.state.UserPrefs))
		for k, v := range sm.state.UserPrefs {
			cp.UserPrefs[k] = v
		}
	}
	return cp
}

// Update applies the given function to the state (in-memory only).
func (sm *StateManager) Update(fn func(*AgentState)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	fn(&sm.state)
}

// SetPref sets a user preference in memory and persists it to the database.
func (sm *StateManager) SetPref(key, value string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.state.UserPrefs == nil {
		sm.state.UserPrefs = make(map[string]string)
	}
	sm.state.UserPrefs[key] = value

	if sm.pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := sm.pool.Exec(ctx,
			`INSERT INTO user_preferences (key, value, updated_at)
			 VALUES ($1, $2, NOW())
			 ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()`,
			key, value)
		if err != nil {
			return fmt.Errorf("persist preference: %w", err)
		}
	}
	return nil
}

// DeletePref removes a user preference from memory and the database.
func (sm *StateManager) DeletePref(key string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	delete(sm.state.UserPrefs, key)

	if sm.pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := sm.pool.Exec(ctx,
			`DELETE FROM user_preferences WHERE key = $1`, key)
		if err != nil {
			return fmt.Errorf("delete preference: %w", err)
		}
	}
	return nil
}

// ReadMessages returns a deep copy of the conversation messages.
func (sm *StateManager) ReadMessages() []llm.Message {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	cp := make([]llm.Message, len(sm.messages))
	copy(cp, sm.messages)
	return cp
}

// AppendMessage appends a message to the conversation context.
func (sm *StateManager) AppendMessage(msg llm.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.messages = append(sm.messages, msg)
}

// MessageCount returns the number of messages in the conversation context.
func (sm *StateManager) MessageCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return len(sm.messages)
}
