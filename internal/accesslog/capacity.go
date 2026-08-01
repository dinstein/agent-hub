package accesslog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const dayLayout = "2006-01-02"

// PruneResult describes whole UTC partitions removed by a retention pass.
type PruneResult struct {
	Days  int      `json:"days"`
	Bytes int64    `json:"bytes"`
	Names []string `json:"names,omitempty"`
}

func retentionCutoff(now time.Time, days int) time.Time {
	today, _ := time.Parse(dayLayout, now.UTC().Format(dayLayout))
	return today.AddDate(0, 0, -(days - 1))
}

func (s *Store) withCapacityLocked(day string, added int64, write func() error) (err error) {
	if s.lock == nil {
		return errors.New("accesslog: capacity lock is closed")
	}
	if err := flockExclusive(s.lock); err != nil {
		return fmt.Errorf("accesslog: lock capacity: %w", err)
	}
	defer func() {
		if unlockErr := flockUnlock(s.lock); err == nil && unlockErr != nil {
			err = fmt.Errorf("accesslog: unlock capacity: %w", unlockErr)
		}
	}()
	if s.retention > 0 {
		cutoff := retentionCutoff(s.clock(), s.retention)
		if day < cutoff.Format(dayLayout) {
			return fmt.Errorf("%w: %s is before %s", ErrExpired, day, cutoff.Format(dayLayout))
		}
		if _, err := pruneUnlocked(s.root, cutoff, false); err != nil {
			return err
		}
	}
	if s.maxBytes > 0 {
		usage, err := Inspect(s.root)
		if err != nil {
			return err
		}
		if added > s.maxBytes-usage.Bytes {
			return fmt.Errorf("%w: %d stored + %d new > %d", ErrCapacity, usage.Bytes, added, s.maxBytes)
		}
	}
	if s.minFree > 0 {
		free, err := freeBytes(s.root)
		if err != nil {
			return fmt.Errorf("accesslog: inspect free space: %w", err)
		}
		if added > free-s.minFree {
			return fmt.Errorf("%w: %d free - %d new < %d", ErrFreeReserve, free, added, s.minFree)
		}
	}
	return write()
}

// Prune removes complete UTC partitions strictly older than cutoff. It uses
// the same cross-process lock as writers and never follows or constructs a
// deletion target from an unvalidated directory name.
func Prune(root string, cutoff time.Time, dryRun bool) (out PruneResult, err error) {
	if !crossProcessLockSupported {
		return out, errors.New("accesslog: prune requires a cross-process lock on this platform")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return out, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".audit.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := flockExclusive(lock); err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, flockUnlock(lock)) }()
	return pruneUnlocked(root, cutoff, dryRun)
}

func pruneUnlocked(root string, cutoff time.Time, dryRun bool) (PruneResult, error) {
	var out PruneResult
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	cutoffDay := cutoff.UTC().Format(dayLayout)
	for _, entry := range entries {
		name := entry.Name()
		parsed, parseErr := time.Parse(dayLayout, name)
		if !entry.IsDir() || parseErr != nil || parsed.Format(dayLayout) != name || name >= cutoffDay {
			continue
		}
		target := filepath.Join(root, name)
		usage, err := Inspect(target)
		if err != nil {
			return out, err
		}
		out.Days++
		out.Bytes += usage.Bytes
		out.Names = append(out.Names, name)
		if !dryRun {
			if err := os.RemoveAll(target); err != nil {
				return out, fmt.Errorf("accesslog: remove expired partition %s: %w", name, err)
			}
		}
	}
	return out, nil
}
