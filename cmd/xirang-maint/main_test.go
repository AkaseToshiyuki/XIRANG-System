package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xirang-grid/xirang/internal/backup"
	"go.etcd.io/bbolt"
)

func TestValidateDatabaseSet(t *testing.T) {
	manifest := standardManifest()
	if err := validateDatabaseSet(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Files[4].Name = manifest.Files[0].Name
	if err := validateDatabaseSet(manifest); err == nil {
		t.Fatal("duplicate database set accepted")
	}
}

func TestRestorePreflightsLocksAndRestoresAllDatabases(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	manifest := standardManifest()
	destinations := map[string]string{
		"xirang.db": "xirang.db", "telemetry.db": "telemetry/telemetry.db",
		"topology.db": "topology/topology.db", "flows.db": "flows/flows.db", "alerts.db": "alerts/alerts.db",
	}
	for _, record := range manifest.Files {
		createTestDatabase(t, filepath.Join(source, record.Name), "new")
		createTestDatabase(t, filepath.Join(destination, destinations[record.Name]), "old")
	}
	lockedPath := filepath.Join(destination, destinations["topology.db"])
	locked, err := bbolt.Open(lockedPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(source, destination, manifest); err == nil {
		t.Fatal("restore overwrote an active database")
	}
	if value := readTestDatabase(t, filepath.Join(destination, destinations["xirang.db"])); value != "old" {
		t.Fatalf("restore changed data before completing lock preflight: %q", value)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restore(source, destination, manifest); err != nil {
		t.Fatal(err)
	}
	for _, record := range manifest.Files {
		if value := readTestDatabase(t, filepath.Join(destination, destinations[record.Name])); value != "new" {
			t.Fatalf("%s was not restored: %q", record.Name, value)
		}
	}
}

func standardManifest() backup.Manifest {
	return backup.Manifest{Files: []backup.FileRecord{
		{Name: "xirang.db"}, {Name: "telemetry.db"}, {Name: "topology.db"}, {Name: "flows.db"}, {Name: "alerts.db"},
	}}
}

func createTestDatabase(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("test"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("value"), []byte(value))
	}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func readTestDatabase(t *testing.T, path string) string {
	t.Helper()
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var value []byte
	if err := database.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("test"))
		if bucket == nil {
			return errors.New("missing test bucket")
		}
		value = append([]byte(nil), bucket.Get([]byte("value"))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return string(value)
}
