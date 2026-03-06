package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"sokratos/logger"
)

// BackgroundJob represents a Brain session handling a complex tool call
// asynchronously while the 9B continues serving the user.
type BackgroundJob struct {
	mu             sync.RWMutex
	ID             string
	Tool           string      // triggering tool ("create_skill") or "reason"
	TaskType       string      // maps to session prompt; "" = general reasoning
	UserGoal       string      // original user message or problem statement
	ChannelID      string      // platform channel for sending messages
	InputCh        chan string // user messages flow in here (cap 1)
	closeOnce      sync.Once   // ensures InputCh is closed exactly once
	isActive       bool        // true during inference, false when parked
	lastQuestion   string      // what the Brain last asked
	toolSucceeded  bool        // set when triggering tool succeeds
	cancelInflight context.CancelFunc
	CreatedAt      time.Time
}

// SetActive marks the job as actively inferring (or not). When active, the
// cancel function can abort mid-inference.
func (j *BackgroundJob) SetActive(active bool, cancel context.CancelFunc) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.isActive = active
	j.cancelInflight = cancel
}

// SetLastQuestion records the Brain's most recent question to the user.
func (j *BackgroundJob) SetLastQuestion(q string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lastQuestion = q
}

// SetToolSucceeded marks that the triggering tool executed successfully.
func (j *BackgroundJob) SetToolSucceeded(v bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.toolSucceeded = v
}

// Snapshot returns a consistent snapshot of the job's mutable state.
func (j *BackgroundJob) Snapshot() (isActive bool, lastQuestion string, toolSucceeded bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.isActive, j.lastQuestion, j.toolSucceeded
}

// Cancel aborts mid-inference (if active) and closes InputCh to unpark
// a waiting goroutine.
func (j *BackgroundJob) Cancel() {
	j.mu.Lock()
	if j.cancelInflight != nil {
		j.cancelInflight()
	}
	j.mu.Unlock()
	// Close InputCh outside the lock — sync.Once guarantees at-most-once.
	j.closeOnce.Do(func() { close(j.InputCh) })
}

// --- StateManager job management ---

// CreateJob creates a new background job and stores it in the state manager.
func (sm *StateManager) CreateJob(tool, userGoal, channelID string) *BackgroundJob {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		b = []byte{0x42, 0x42, 0x42} // fallback
	}
	id := hex.EncodeToString(b)

	job := &BackgroundJob{
		ID:        id,
		Tool:      tool,
		UserGoal:  userGoal,
		ChannelID: channelID,
		InputCh:   make(chan string, 1),
		CreatedAt: time.Now(),
	}

	sm.jobsMu.Lock()
	sm.jobs[id] = job
	sm.jobsMu.Unlock()

	logger.Log.Infof("[jobs] created job %s for tool %s", id, tool)
	return job
}

// GetJob returns a background job by ID, or nil if not found.
func (sm *StateManager) GetJob(id string) *BackgroundJob {
	sm.jobsMu.RLock()
	defer sm.jobsMu.RUnlock()
	return sm.jobs[id]
}

// GetJobs returns all active background jobs.
func (sm *StateManager) GetJobs() []*BackgroundJob {
	sm.jobsMu.RLock()
	defer sm.jobsMu.RUnlock()
	out := make([]*BackgroundJob, 0, len(sm.jobs))
	for _, j := range sm.jobs {
		out = append(out, j)
	}
	return out
}

// RemoveJob deletes a background job from the state manager.
func (sm *StateManager) RemoveJob(id string) {
	sm.jobsMu.Lock()
	delete(sm.jobs, id)
	sm.jobsMu.Unlock()
	logger.Log.Infof("[jobs] removed job %s", id)
}
