package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Save atomically writes the index to path as JSON. It writes to a temporary
// file in the same directory and renames it into place so readers never observe
// a partial file. An empty path is a no-op.
func Save(idx *Index, path string) error {
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create snapshot temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)

		return fmt.Errorf("write snapshot temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("close snapshot temp: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("rename snapshot: %w", err)
	}

	return nil
}

// LoadSnapshot reads a previously saved index from path. It returns (nil, nil)
// when path is empty or the file does not exist, so a cold start is not an error.
func LoadSnapshot(path string) (*Index, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read snapshot %q: %w", path, err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse snapshot %q: %w", path, err)
	}

	return &idx, nil
}
