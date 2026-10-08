package main

import (
	"errors"
	"path/filepath"
)

// Resolve CLI paths once, before deriving state-file locations or opening any
// stores. Backup storage deliberately requires a clean absolute root.
func resolveStateDirectories(data, backups string) (string, string, error) {
	if data == "" {
		return "", "", errors.New("data-dir must not be empty")
	}
	data, err := filepath.Abs(data)
	if err != nil {
		return "", "", err
	}
	if backups == "" {
		backups = filepath.Join(data, "backups")
	}
	backups, err = filepath.Abs(backups)
	if err != nil {
		return "", "", err
	}
	return data, backups, nil
}
