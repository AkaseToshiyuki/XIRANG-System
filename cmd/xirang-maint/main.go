package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xirang-grid/xirang/internal/backup"
	"github.com/xirang-grid/xirang/pkg/version"
	"go.etcd.io/bbolt"
)

type unusedSource struct{}

func (unusedSource) Snapshot(context.Context, io.Writer) error { return errors.New("unused") }

type replacement struct {
	name, destination, temporary string
	existed                      bool
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Printf("%s maintenance %s\n", version.Name, version.Version)
		return
	}
	if len(os.Args) < 3 || (os.Args[1] != "verify" && os.Args[1] != "restore") {
		fmt.Fprintln(os.Stderr, "usage: xirang-maint verify <backup-directory> | restore <backup-directory> <data-directory> | version")
		os.Exit(2)
	}
	backupPath, err := filepath.Abs(os.Args[2])
	if err != nil {
		fail(err)
	}
	encoded, err := os.ReadFile(filepath.Join(backupPath, "manifest.json"))
	if err != nil {
		fail(err)
	}
	var manifest backup.Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		fail(err)
	}
	if err := validateDatabaseSet(manifest); err != nil {
		fail(err)
	}
	sources := make(map[string]backup.SnapshotSource, len(manifest.Files))
	for _, record := range manifest.Files {
		sources[record.Name] = unusedSource{}
	}
	service, err := backup.New(filepath.Dir(backupPath), sources)
	if err != nil {
		fail(err)
	}
	verified, err := service.Verify(context.Background(), filepath.Base(backupPath))
	if err != nil {
		fail(err)
	}
	if os.Args[1] == "verify" {
		fmt.Printf("%s verified: %d databases, schema %d\n", verified.ID, len(verified.Files), verified.SchemaVersion)
		return
	}
	if len(os.Args) != 4 {
		fail(errors.New("restore requires a destination data directory"))
	}
	dataDirectory, err := filepath.Abs(os.Args[3])
	if err != nil {
		fail(err)
	}
	if err := restore(backupPath, dataDirectory, verified); err != nil {
		fail(err)
	}
	fmt.Printf("%s restored into %s\n", verified.ID, dataDirectory)
}

func restore(source, dataDirectory string, manifest backup.Manifest) error {
	destinations := map[string]string{
		"xirang.db": "xirang.db", "telemetry.db": "telemetry/telemetry.db",
		"topology.db": "topology/topology.db", "flows.db": "flows/flows.db", "alerts.db": "alerts/alerts.db",
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	rollback := filepath.Join(dataDirectory, ".restore-rollback-"+stamp)
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		return err
	}
	replacements := make([]replacement, 0, len(manifest.Files))
	defer func() {
		for _, replacement := range replacements {
			_ = os.Remove(replacement.temporary)
		}
	}()
	for _, record := range manifest.Files {
		relative, ok := destinations[record.Name]
		if !ok {
			return fmt.Errorf("backup contains unknown database %q", record.Name)
		}
		destination := filepath.Join(dataDirectory, relative)
		replacement := replacement{
			name: record.Name, destination: destination,
			temporary: destination + ".restore-" + stamp + ".tmp",
		}
		if _, err := os.Stat(destination); err == nil {
			database, err := bbolt.Open(destination, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond})
			if err != nil {
				return fmt.Errorf("destination is locked; stop all controllers before restore: %w", err)
			}
			_ = database.Close()
			replacement.existed = true
			if err := copyFile(destination, filepath.Join(rollback, record.Name), 0o600); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(source, record.Name), replacement.temporary, 0o600); err != nil {
			return err
		}
		replacements = append(replacements, replacement)
	}
	for index, replacement := range replacements {
		if err := os.Rename(replacement.temporary, replacement.destination); err != nil {
			rollbackErr := rollbackReplacements(replacements[:index], rollback)
			return errors.Join(err, rollbackErr)
		}
		if err := syncDirectory(filepath.Dir(replacement.destination)); err != nil {
			rollbackErr := rollbackReplacements(replacements[:index+1], rollback)
			return errors.Join(err, rollbackErr)
		}
	}
	return syncDirectory(rollback)
}

func rollbackReplacements(replacements []replacement, rollback string) error {
	var result error
	for index := len(replacements) - 1; index >= 0; index-- {
		replacement := replacements[index]
		if replacement.existed {
			temporary := replacement.destination + ".rollback." + filepath.Base(rollback) + ".tmp"
			if err := copyFile(filepath.Join(rollback, replacement.name), temporary, 0o600); err != nil {
				result = errors.Join(result, err)
				continue
			}
			if err := os.Rename(temporary, replacement.destination); err != nil {
				result = errors.Join(result, err)
			}
		} else if err := os.Remove(replacement.destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		if err := syncDirectory(filepath.Dir(replacement.destination)); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func validateDatabaseSet(manifest backup.Manifest) error {
	expected := map[string]bool{
		"xirang.db": false, "telemetry.db": false, "topology.db": false, "flows.db": false, "alerts.db": false,
	}
	if len(manifest.Files) != len(expected) {
		return errors.New("backup must contain the five XIRANG databases")
	}
	for _, record := range manifest.Files {
		if _, ok := expected[record.Name]; !ok || expected[record.Name] {
			return fmt.Errorf("backup contains unknown or duplicate database %q", record.Name)
		}
		expected[record.Name] = true
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
