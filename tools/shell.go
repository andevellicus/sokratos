package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sokratos/logger"
)

// ShellExec manages shell command execution with an allowlist, workspace
// constraints, and audit logging.
type ShellExec struct {
	mu           sync.RWMutex
	config       *ShellConfig
	pool         *pgxpool.Pool
	workspaceDir string // absolute path
	configPath   string
	lastMtime    time.Time
}

// NewShellExec creates a ShellExec with the given config path and workspace
// directory. The workspace dir is resolved to an absolute path.
func NewShellExec(pool *pgxpool.Pool, workspaceDir, configPath string) (*ShellExec, error) {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace dir: %w", err)
	}

	cfg, err := loadShellConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load shell config: %w", err)
	}

	se := &ShellExec{
		config:       cfg,
		pool:         pool,
		workspaceDir: absWorkspace,
		configPath:   configPath,
	}

	// Record initial mtime.
	if info, err := os.Stat(configPath); err == nil {
		se.lastMtime = info.ModTime()
	}

	logger.Log.Infof("[shell] loaded %d commands from %s (workspace: %s)", len(cfg.Commands), configPath, absWorkspace)
	return se, nil
}

// ToolFunc returns the closure for registry registration.
func (se *ShellExec) ToolFunc() ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Command    string `json:"command"`
			WorkingDir string `json:"working_dir"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", Errorf("invalid arguments: %v", err)
		}
		if strings.TrimSpace(req.Command) == "" {
			return "", Errorf("command is required")
		}
		return se.execute(ctx, req.Command, req.WorkingDir)
	}
}

// SyncIfChanged reloads the config if the file's mtime has changed.
func (se *ShellExec) SyncIfChanged() {
	info, err := os.Stat(se.configPath)
	if err != nil {
		return
	}
	if !info.ModTime().After(se.lastMtime) {
		return
	}

	cfg, err := loadShellConfig(se.configPath)
	if err != nil {
		logger.Log.Warnf("[shell] failed to reload config: %v", err)
		return
	}

	se.mu.Lock()
	se.config = cfg
	se.lastMtime = info.ModTime()
	se.mu.Unlock()

	logger.Log.Infof("[shell] reloaded config: %d commands", len(cfg.Commands))
}

// CommandDescriptions returns a summary of available commands for prompt injection.
func (se *ShellExec) CommandDescriptions() string {
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.config.CommandDescriptions()
}

// execute runs a shell command with allowlist checks, workspace constraints,
// timeout enforcement, and audit logging.
func (se *ShellExec) execute(ctx context.Context, command, workingDir string) (string, error) {
	se.mu.RLock()
	cfg := se.config
	se.mu.RUnlock()

	// Extract all binary names from the (potentially compound) command.
	binaries := extractBinaries(command)
	if len(binaries) == 0 {
		return "", Errorf("could not parse command binary")
	}

	// Allowlist check: every binary must be allowed.
	for _, b := range binaries {
		if !cfg.IsAllowed(b) {
			return fmt.Sprintf("Command %q is not in the allowlist.\n%s", b, cfg.CommandDescriptions()), nil
		}
	}

	// Resolve timeout (max across all binaries) and workspace constraint
	// (most restrictive — if any binary requires workspace_only, enforce it).
	timeout := cfg.ResolvedTimeout(binaries[0])
	wsOnly := cfg.ResolvedWorkspaceOnly(binaries[0])
	for _, b := range binaries[1:] {
		if t := cfg.ResolvedTimeout(b); t > timeout {
			timeout = t
		}
		if cfg.ResolvedWorkspaceOnly(b) {
			wsOnly = true
		}
	}

	// Resolve working directory.
	resolvedDir, err := se.resolveWorkDir(wsOnly, workingDir)
	if err != nil {
		return err.Error(), nil // soft error
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build command.
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = resolvedDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	// Kill process group on context cancellation.
	if cmdCtx.Err() != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if cmdCtx.Err() != nil {
			// Timeout — return soft error.
			se.auditAsync(command, resolvedDir, -1, "", runErr.Error(), duration)
			return fmt.Sprintf("Command timed out after %s (killed).\n\n--- stderr ---\n%s",
				timeout, truncateOutput(stderr.String(), 500)), nil
		} else {
			// exec.Command creation failure — soft error so the LLM can retry
			// with corrected parameters (e.g. invalid working_dir).
			return fmt.Sprintf("Command execution failed: %v", runErr), nil
		}
	}

	// Truncate output.
	maxOutput := cfg.ResolvedMaxOutput()
	stdoutStr := truncateOutput(stdout.String(), maxOutput)
	stderrStr := truncateOutput(stderr.String(), maxOutput)

	// Audit (fire-and-forget).
	se.auditAsync(command, resolvedDir, exitCode,
		truncateOutput(stdout.String(), 500),
		truncateOutput(stderr.String(), 500),
		duration)

	// Format result.
	var result strings.Builder
	fmt.Fprintf(&result, "Exit code: %d | Duration: %s\n", exitCode, duration.Truncate(time.Millisecond))

	if stdoutStr != "" {
		result.WriteString("\n--- stdout ---\n")
		result.WriteString(stdoutStr)
	}
	if stderrStr != "" {
		result.WriteString("\n--- stderr ---\n")
		result.WriteString(stderrStr)
	}

	if stdoutStr == "" && stderrStr == "" {
		result.WriteString("\n(no output)")
	}

	return result.String(), nil
}

// resolveWorkDir determines the effective working directory for a command.
func (se *ShellExec) resolveWorkDir(wsOnly bool, workingDir string) (string, error) {
	if wsOnly {
		// Workspace-constrained: resolve relative to workspace dir.
		resolved := se.workspaceDir
		if workingDir != "" {
			resolved = filepath.Join(se.workspaceDir, filepath.Clean(workingDir))
		}
		// Ensure it stays within workspace (prevent ../ escape).
		absResolved, err := filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("failed to resolve working directory: %v", err)
		}
		if !strings.HasPrefix(absResolved, se.workspaceDir) {
			return "", fmt.Errorf("working directory %q escapes workspace (must be within %s)", workingDir, se.workspaceDir)
		}
		return absResolved, nil
	}

	// Not workspace-constrained.
	if workingDir == "" {
		return se.workspaceDir, nil
	}
	workingDir = expandTilde(workingDir)
	if filepath.IsAbs(workingDir) {
		return workingDir, nil
	}
	// Relative to CWD.
	return filepath.Abs(workingDir)
}

// auditAsync inserts a record into shell_history in a fire-and-forget goroutine.
func (se *ShellExec) auditAsync(command, workDir string, exitCode int, stdoutPreview, stderrPreview string, duration time.Duration) {
	if se.pool == nil {
		return
	}
	pool := se.pool
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), TimeoutShellAudit)
		defer cancel()
		_, err := pool.Exec(ctx,
			`INSERT INTO shell_history (command, working_dir, exit_code, stdout_preview, stderr_preview, duration_ms)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			command, workDir, exitCode, stdoutPreview, stderrPreview, duration.Milliseconds())
		if err != nil {
			logger.Log.Warnf("[shell] audit insert failed: %v", err)
		}
	}()
}

// extractBinaries returns the base names of all command binaries in a
// potentially compound command (split on &&, ||, ;, |). Deduplicates while
// preserving order.
func extractBinaries(command string) []string {
	segments := splitShellCompound(command)
	var binaries []string
	seen := make(map[string]bool)
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		b := filepath.Base(fields[0])
		if b != "" && !seen[b] {
			binaries = append(binaries, b)
			seen[b] = true
		}
	}
	return binaries
}

// splitShellCompound splits a command string on shell compound operators
// (&&, ||, ;, |) returning the individual command segments.
func splitShellCompound(command string) []string {
	// Replace multi-char operators first so single-char variants don't
	// partially match them.
	const sep = "\x00"
	s := strings.ReplaceAll(command, "&&", sep)
	s = strings.ReplaceAll(s, "||", sep)
	s = strings.ReplaceAll(s, ";", sep)
	s = strings.ReplaceAll(s, "|", sep)

	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// truncateOutput truncates a string to maxLen characters, appending "..." if truncated.
func truncateOutput(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}
