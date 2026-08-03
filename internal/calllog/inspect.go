package calllog

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Usage is a bounded metadata-only inventory of the ledger directory.
type Usage struct {
	Bytes      int64 `json:"bytes"`
	Days       int   `json:"days"`
	EventFiles int   `json:"eventFiles"`
	PackFiles  int   `json:"packFiles"`
}

// Inspect walks the ledger directory without following symlinks.
func Inspect(root string) (Usage, error) {
	var out Usage
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if filepath.Dir(path) == root && len(entry.Name()) == len("2006-01-02") {
				out.Days++
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out.Bytes += info.Size()
		switch {
		case entry.Name() == EventFileName, entry.Name() == LegacyEventFileName:
			out.EventFiles++
		case strings.HasSuffix(entry.Name(), ".pack"):
			out.PackFiles++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return Usage{}, nil
	}
	return out, err
}
