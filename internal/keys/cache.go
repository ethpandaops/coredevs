package keys

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// entryFileExt is the on-disk suffix for a per-handle cache file.
const entryFileExt = ".json"

// loadCache reads every per-handle entry file from dir into a map keyed by
// lowercased handle. It returns an empty map when dir is empty or absent, so a
// cold start is not an error.
func loadCache(dir string) (map[string]*Entry, error) {
	out := make(map[string]*Entry, 0)
	if dir == "" {
		return out, nil
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}

		return nil, fmt.Errorf("read keys cache dir %q: %w", dir, err)
	}

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), entryFileExt) {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, fmt.Errorf("read keys cache entry %q: %w", f.Name(), err)
		}

		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("parse keys cache entry %q: %w", f.Name(), err)
		}

		out[strings.TrimSuffix(f.Name(), entryFileExt)] = &e
	}

	return out, nil
}

// writeEntry atomically persists a single handle's entry to dir/<key>.json via a
// temp file and rename so readers never observe a partial file, and so a restart
// mid-pass keeps every handle fetched so far. An empty dir is a no-op.
func writeEntry(dir, key string, e Entry) error {
	if dir == "" {
		return nil
	}

	name, ok := entryFileName(key)
	if !ok {
		return fmt.Errorf("unsafe cache key %q", key)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create keys cache dir: %w", err)
	}

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keys cache entry: %w", err)
	}

	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create keys cache temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)

		return fmt.Errorf("write keys cache temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("close keys cache temp: %w", err)
	}

	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("rename keys cache entry: %w", err)
	}

	return nil
}

// removeEntry deletes a handle's cache file. A missing file or empty dir is not
// an error.
func removeEntry(dir, key string) error {
	if dir == "" {
		return nil
	}

	name, ok := entryFileName(key)
	if !ok {
		return nil
	}

	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove keys cache entry: %w", err)
	}

	return nil
}

// entryFileName returns the on-disk filename for a cache key, rejecting any key
// that is not a plain GitHub-login-shaped token so it can never escape the dir.
func entryFileName(key string) (string, bool) {
	if key == "" {
		return "", false
	}

	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", false
		}
	}

	return key + entryFileExt, true
}
