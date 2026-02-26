package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/mem"

	"sokratos/logger"
)

// ManagedModel represents an LLM model whose VRAM lifecycle is managed by the auditor.
type ManagedModel struct {
	Name       string // logical name: "deep_thinker", "text2sql"
	ServerURL  string
	ModelID    string    // exact model name for API
	EstimateMB int       // VRAM estimate for logging
	LastAccess time.Time // zero value means not accessed / already evicted
}

// VRAMAuditor tracks access times for managed models and evicts idle or
// pressure-triggered models via the llama.cpp router's POST /models/unload API.
type VRAMAuditor struct {
	mu                sync.Mutex
	models            map[string]*ManagedModel
	client            *http.Client
	pressureThreshold float64 // available memory % below which we force-evict (default 15.0)
}

// NewVRAMAuditor creates a VRAMAuditor with a shared HTTP client.
// pressureThreshold is the available-memory percentage below which all managed
// models are force-evicted (e.g. 15.0 means evict when < 15% memory is free).
func NewVRAMAuditor(pressureThreshold float64) *VRAMAuditor {
	return &VRAMAuditor{
		models:            make(map[string]*ManagedModel),
		client:            &http.Client{Timeout: 10 * time.Second},
		pressureThreshold: pressureThreshold,
	}
}

// Register adds a model to be managed by the auditor.
func (v *VRAMAuditor) Register(name, serverURL, modelID string, estimateMB int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.models[name] = &ManagedModel{
		Name:       name,
		ServerURL:  serverURL,
		ModelID:    modelID,
		EstimateMB: estimateMB,
	}
	logger.Log.Infof("[vram_auditor] registered managed model %q (%s, ~%d MB)", name, modelID, estimateMB)
}

// UpdateAccessTime records a fresh access for the named model.
func (v *VRAMAuditor) UpdateAccessTime(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if m, ok := v.models[name]; ok {
		m.LastAccess = time.Now()
		logger.Log.Debugf("[vram_auditor] UpdateAccessTime(%q)", name)
	}
}

type unloadRequest struct {
	Model string `json:"model"`
}

// EvictModel sends POST {serverURL}/models/unload to evict the named model.
// Returns nil on success, error on failure. Non-fatal — caller logs and continues.
func (v *VRAMAuditor) EvictModel(name string) error {
	v.mu.Lock()
	m, ok := v.models[name]
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("model %q not registered", name)
	}
	serverURL := m.ServerURL
	modelID := m.ModelID
	estimateMB := m.EstimateMB
	v.mu.Unlock()

	body, err := json.Marshal(unloadRequest{Model: modelID})
	if err != nil {
		return fmt.Errorf("marshal unload request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, serverURL+"/models/unload", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create unload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("unload request failed for %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload %q returned status %d", name, resp.StatusCode)
	}

	logger.Log.Infof("[vram_auditor] evicted %q (%s, ~%d MB reclaimed)", name, modelID, estimateMB)
	return nil
}

// Run starts two background ticker loops: idle eviction (every 60s) and memory
// pressure detection (every 30s). It blocks until ctx is cancelled.
func (v *VRAMAuditor) Run(ctx context.Context) {
	logger.Log.Infof("[vram_auditor] started (idle=10m, pressure_check=30s, threshold=%.1f%%)", v.pressureThreshold)

	idleTicker := time.NewTicker(60 * time.Second)
	pressureTicker := time.NewTicker(30 * time.Second)
	defer idleTicker.Stop()
	defer pressureTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Log.Info("[vram_auditor] shutting down")
			return

		case <-idleTicker.C:
			v.evictIdle()

		case <-pressureTicker.C:
			v.checkMemoryPressure()
		}
	}
}

func (v *VRAMAuditor) evictIdle() {
	v.mu.Lock()
	var toEvict []string
	for name, m := range v.models {
		if !m.LastAccess.IsZero() && time.Since(m.LastAccess) > 10*time.Minute {
			toEvict = append(toEvict, name)
		}
	}
	v.mu.Unlock()

	for _, name := range toEvict {
		if err := v.EvictModel(name); err != nil {
			logger.Log.Warnf("[vram_auditor] idle eviction failed for %q: %v", name, err)
		}
		// Reset LastAccess to zero so we don't re-evict on next tick.
		v.mu.Lock()
		if m, ok := v.models[name]; ok {
			m.LastAccess = time.Time{}
		}
		v.mu.Unlock()
	}
}

func (v *VRAMAuditor) checkMemoryPressure() {
	vm, err := mem.VirtualMemory()
	if err != nil {
		logger.Log.Warnf("[vram_auditor] failed to read memory stats: %v", err)
		return
	}

	availPct := 100.0 - vm.UsedPercent
	if availPct < v.pressureThreshold {
		logger.Log.Warnf("[vram_auditor] MEMORY PRESSURE: %.1f%% available (threshold %.1f%%), total=%dMB used=%dMB — evicting all managed models",
			availPct, v.pressureThreshold, vm.Total/(1024*1024), vm.Used/(1024*1024))

		v.mu.Lock()
		var names []string
		for name := range v.models {
			names = append(names, name)
		}
		v.mu.Unlock()

		for _, name := range names {
			if err := v.EvictModel(name); err != nil {
				logger.Log.Warnf("[vram_auditor] pressure eviction failed for %q: %v", name, err)
			}
			v.mu.Lock()
			if m, ok := v.models[name]; ok {
				m.LastAccess = time.Time{}
			}
			v.mu.Unlock()
		}
	}
}
