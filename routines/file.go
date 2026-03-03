package routines

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/BurntSushi/toml"

	"sokratos/logger"
)

// FileWriter writes routine entries to the routines TOML file.
type FileWriter interface {
	Write(name string, entry Entry) error
	Delete(name string)
}

// FileAdapter is the default FileWriter backed by a TOML file on disk.
type FileAdapter struct {
	Path string
	mu   sync.Mutex
}

// Write adds or updates a routine in the TOML file.
func (a *FileAdapter) Write(name string, entry Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	routines, err := LoadFile(a.Path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read existing: %w", err)
	}
	if routines == nil {
		routines = make(map[string]Entry)
	}

	routines[name] = entry
	return writeFile(a.Path, routines)
}

// Delete removes a routine from the TOML file.
func (a *FileAdapter) Delete(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	routines, err := LoadFile(a.Path)
	if err != nil {
		return
	}
	delete(routines, name)
	if err := writeFile(a.Path, routines); err != nil {
		logger.Log.Warnf("[routines] failed to write file after delete: %v", err)
	}
}

// LoadFile reads and parses a routines TOML file into a map of name -> Entry.
func LoadFile(path string) (map[string]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var routines map[string]Entry
	if err := toml.Unmarshal(data, &routines); err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	return routines, nil
}

// writeFile serializes routines to TOML and writes to disk.
func writeFile(path string, routines map[string]Entry) error {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(routines); err != nil {
		return fmt.Errorf("encode TOML: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}
