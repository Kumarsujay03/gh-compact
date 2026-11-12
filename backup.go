package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Backup records everything a run changes, so `gh compact restore` can undo it.
type Backup struct {
	Version   int          `json:"version"`
	CreatedAt time.Time    `json:"created_at"`
	Repos     []RepoBackup `json:"repos"`
}

type RepoBackup struct {
	Repo   string        `json:"repo"`
	Groups []GroupBackup `json:"groups"`
}

type GroupBackup struct {
	Survivor Release   `json:"survivor"` // with its body before compaction
	Deleted  []Release `json:"deleted"`
}

func backupDir() (string, error) {
	if d := os.Getenv("GH_COMPACT_BACKUP_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gh-compact"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "gh-compact"), nil
}

func writeBackup(b Backup) (string, error) {
	dir, err := backupDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "backup-"+b.CreatedAt.Format("20060102-150405")+".json")
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o600)
}

func readBackup(path string) (Backup, error) {
	var b Backup
	data, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("%s is not a gh-compact backup: %w", path, err)
	}
	return b, nil
}

func latestBackup() (string, error) {
	dir, err := backupDir()
	if err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "backup-*.json"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no backups in %s", dir)
	}
	return matches[len(matches)-1], nil // names sort by time
}
