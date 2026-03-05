package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxReadFileSize = 50 * 1024 // 50KB
const maxListEntries = 500

// validatePath resolves a user-supplied path relative to the workspace and
// ensures it stays within bounds. Returns the resolved absolute path or a
// soft ToolError on violation.
func validatePath(workspaceDir, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", Errorf("path is required")
	}

	var resolved string
	if filepath.IsAbs(path) {
		resolved = filepath.Clean(path)
	} else {
		resolved = filepath.Join(workspaceDir, path)
	}

	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", Errorf("failed to resolve path: %v", err)
	}

	if !strings.HasPrefix(abs, workspaceDir) {
		return "", Errorf("path %q is outside the workspace (%s)", path, workspaceDir)
	}

	return abs, nil
}

// NewReadFile returns a ToolFunc that reads a file within the workspace.
// Supports optional offset (1-based line number) and limit (line count).
func NewReadFile(workspaceDir string) ToolFunc {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		absWorkspace = workspaceDir
	}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", Errorf("invalid arguments: %v", err)
		}

		resolved, err := validatePath(absWorkspace, req.Path)
		if err != nil {
			return "", err
		}

		info, err := os.Stat(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("File not found: %s", req.Path), nil
			}
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied: %s", req.Path), nil
			}
			return "", Errorf("stat failed: %v", err)
		}
		if info.IsDir() {
			return fmt.Sprintf("%s is a directory, not a file", req.Path), nil
		}
		if info.Size() > maxReadFileSize && req.Offset == 0 && req.Limit == 0 {
			return fmt.Sprintf("File too large (%d bytes, max %d). Use offset/limit to read a portion.", info.Size(), maxReadFileSize), nil
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied: %s", req.Path), nil
			}
			return "", Errorf("read failed: %v", err)
		}

		lines := strings.Split(string(data), "\n")

		// Apply offset (1-based).
		startLine := 1
		if req.Offset > 0 {
			startLine = req.Offset
		}
		if startLine > len(lines) {
			return fmt.Sprintf("Offset %d exceeds file length (%d lines)", startLine, len(lines)), nil
		}

		endLine := len(lines)
		if req.Limit > 0 {
			endLine = startLine - 1 + req.Limit
			if endLine > len(lines) {
				endLine = len(lines)
			}
		}

		// Build numbered output.
		var b strings.Builder
		for i := startLine - 1; i < endLine; i++ {
			fmt.Fprintf(&b, "%4d │ %s\n", i+1, lines[i])
		}

		result := b.String()
		if len(result) > maxReadFileSize {
			result = result[:maxReadFileSize] + "\n... (truncated)"
		}

		return result, nil
	}
}

// NewWriteFile returns a ToolFunc that writes content to a file within the
// workspace. Creates parent directories as needed. Writes atomically via
// temp file + rename.
func NewWriteFile(workspaceDir string) ToolFunc {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		absWorkspace = workspaceDir
	}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", Errorf("invalid arguments: %v", err)
		}

		resolved, err := validatePath(absWorkspace, req.Path)
		if err != nil {
			return "", err
		}

		dir := filepath.Dir(resolved)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied creating directory: %s", dir), nil
			}
			return "", Errorf("mkdir failed: %v", err)
		}

		// Atomic write: temp file in same directory, then rename.
		tmp, err := os.CreateTemp(dir, ".sokratos-write-*")
		if err != nil {
			return "", Errorf("create temp file: %v", err)
		}
		tmpName := tmp.Name()

		_, writeErr := tmp.WriteString(req.Content)
		closeErr := tmp.Close()
		if writeErr != nil {
			os.Remove(tmpName)
			return "", Errorf("write failed: %v", writeErr)
		}
		if closeErr != nil {
			os.Remove(tmpName)
			return "", Errorf("close failed: %v", closeErr)
		}

		if err := os.Rename(tmpName, resolved); err != nil {
			os.Remove(tmpName)
			return "", Errorf("rename failed: %v", err)
		}

		return fmt.Sprintf("Wrote %d bytes to %s", len(req.Content), req.Path), nil
	}
}

// NewListFiles returns a ToolFunc that lists files in a workspace directory.
// Supports optional glob pattern filtering and recursive traversal.
func NewListFiles(workspaceDir string) ToolFunc {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		absWorkspace = workspaceDir
	}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path      string `json:"path"`
			Pattern   string `json:"pattern"`
			Recursive bool   `json:"recursive"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", Errorf("invalid arguments: %v", err)
		}

		resolved, err := validatePath(absWorkspace, req.Path)
		if err != nil {
			return "", err
		}

		info, err := os.Stat(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("Directory not found: %s", req.Path), nil
			}
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied: %s", req.Path), nil
			}
			return "", Errorf("stat failed: %v", err)
		}
		if !info.IsDir() {
			return fmt.Sprintf("%s is a file, not a directory", req.Path), nil
		}

		type entry struct {
			name  string
			size  int64
			isDir bool
		}

		var entries []entry
		count := 0

		if req.Recursive {
			err = filepath.WalkDir(resolved, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil // skip errors
				}
				if path == resolved {
					return nil // skip root
				}
				if count >= maxListEntries {
					return filepath.SkipAll
				}

				rel, _ := filepath.Rel(resolved, path)
				if req.Pattern != "" {
					matched, mErr := filepath.Match(req.Pattern, filepath.Base(path))
					if mErr != nil || !matched {
						if d.IsDir() {
							return nil // keep walking into dirs
						}
						return nil
					}
				}

				info, iErr := d.Info()
				var size int64
				if iErr == nil {
					size = info.Size()
				}

				entries = append(entries, entry{name: rel, size: size, isDir: d.IsDir()})
				count++
				return nil
			})
			if err != nil {
				return "", Errorf("walk failed: %v", err)
			}
		} else {
			dirEntries, err := os.ReadDir(resolved)
			if err != nil {
				if os.IsPermission(err) {
					return fmt.Sprintf("Permission denied: %s", req.Path), nil
				}
				return "", Errorf("readdir failed: %v", err)
			}
			for _, d := range dirEntries {
				if count >= maxListEntries {
					break
				}
				if req.Pattern != "" {
					matched, mErr := filepath.Match(req.Pattern, d.Name())
					if mErr != nil || !matched {
						continue
					}
				}
				info, iErr := d.Info()
				var size int64
				if iErr == nil {
					size = info.Size()
				}
				entries = append(entries, entry{name: d.Name(), size: size, isDir: d.IsDir()})
				count++
			}
		}

		if len(entries) == 0 {
			if req.Pattern != "" {
				return fmt.Sprintf("No files matching %q in %s", req.Pattern, req.Path), nil
			}
			return fmt.Sprintf("Directory %s is empty", req.Path), nil
		}

		var b strings.Builder
		for _, e := range entries {
			typ := "file"
			if e.isDir {
				typ = "dir "
			}
			name := e.name
			if e.isDir {
				name += "/"
			}
			fmt.Fprintf(&b, "%s  %8d  %s\n", typ, e.size, name)
		}
		if count >= maxListEntries {
			fmt.Fprintf(&b, "\n... (capped at %d entries)", maxListEntries)
		}

		return b.String(), nil
	}
}

// NewPatchFile returns a ToolFunc that performs a find-and-replace on a file
// within the workspace. Fails if old_string is not found or matches more
// than once (ambiguous).
func NewPatchFile(workspaceDir string) ToolFunc {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		absWorkspace = workspaceDir
	}

	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", Errorf("invalid arguments: %v", err)
		}

		if req.OldString == "" {
			return "", Errorf("old_string is required")
		}

		resolved, err := validatePath(absWorkspace, req.Path)
		if err != nil {
			return "", err
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Sprintf("File not found: %s", req.Path), nil
			}
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied: %s", req.Path), nil
			}
			return "", Errorf("read failed: %v", err)
		}

		content := string(data)
		count := strings.Count(content, req.OldString)

		if count == 0 {
			return fmt.Sprintf("old_string not found in %s", req.Path), nil
		}
		if count > 1 {
			return fmt.Sprintf("old_string matches %d times in %s (ambiguous — provide more context to make it unique)", count, req.Path), nil
		}

		newContent := strings.Replace(content, req.OldString, req.NewString, 1)

		if err := os.WriteFile(resolved, []byte(newContent), 0o644); err != nil {
			if os.IsPermission(err) {
				return fmt.Sprintf("Permission denied writing: %s", req.Path), nil
			}
			return "", Errorf("write failed: %v", err)
		}

		return fmt.Sprintf("Patched %s (replaced 1 occurrence)", req.Path), nil
	}
}
