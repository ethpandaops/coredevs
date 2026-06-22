package keys

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// saveSnapshot atomically writes the cache to path as JSON, via a temp file and
// rename so readers never observe a partial file. An empty path is a no-op.
func saveSnapshot(entries map[string]Entry, path string) error {
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create keys snapshot dir: %w", err)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keys snapshot: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create keys snapshot temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)

		return fmt.Errorf("write keys snapshot temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("close keys snapshot temp: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("rename keys snapshot: %w", err)
	}

	return nil
}

// loadSnapshot reads a previously saved cache from path. It returns (nil, nil)
// when path is empty or the file does not exist, so a cold start is not an error.
func loadSnapshot(path string) (map[string]*Entry, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read keys snapshot %q: %w", path, err)
	}

	var stored map[string]Entry
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse keys snapshot %q: %w", path, err)
	}

	out := make(map[string]*Entry, len(stored))
	for key, e := range stored {
		entry := e
		out[key] = &entry
	}

	return out, nil
}
