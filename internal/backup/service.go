package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

var (
	ErrInvalid  = errors.New("invalid backup request")
	ErrNotFound = errors.New("backup not found")
)

type SnapshotSource interface {
	Snapshot(context.Context, io.Writer) error
}

type FileRecord struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	ID            string       `json:"id"`
	SchemaVersion int          `json:"schemaVersion"`
	CreatedAt     time.Time    `json:"createdAt"`
	Files         []FileRecord `json:"files"`
}

type Service struct {
	root    string
	sources map[string]SnapshotSource
	now     func() time.Time
}

func New(root string, sources map[string]SnapshotSource) (*Service, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || len(sources) == 0 {
		return nil, ErrInvalid
	}
	cloned := make(map[string]SnapshotSource, len(sources))
	for name, source := range sources {
		if source == nil || name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".db") {
			return nil, ErrInvalid
		}
		cloned[name] = source
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Service{root: root, sources: cloned, now: time.Now}, nil
}

func (service *Service) Create(ctx context.Context) (Manifest, error) {
	randomBytes := make([]byte, 6)
	if _, err := rand.Read(randomBytes); err != nil {
		return Manifest{}, err
	}
	now := service.now().UTC()
	id := "backup-" + now.Format("20060102T150405Z") + "-" + hex.EncodeToString(randomBytes)
	temporary := filepath.Join(service.root, "."+id+".tmp")
	final := filepath.Join(service.root, id)
	if err := os.Mkdir(temporary, 0o700); err != nil {
		return Manifest{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporary)
		}
	}()
	names := make([]string, 0, len(service.sources))
	for name := range service.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	manifest := Manifest{ID: id, SchemaVersion: 1, CreatedAt: now}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		path := filepath.Join(temporary, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return Manifest{}, err
		}
		hash := sha256.New()
		err = service.sources[name].Snapshot(ctx, io.MultiWriter(file, hash))
		syncErr := file.Sync()
		closeErr := file.Close()
		if err != nil {
			return Manifest{}, err
		}
		if syncErr != nil {
			return Manifest{}, syncErr
		}
		if closeErr != nil {
			return Manifest{}, closeErr
		}
		info, err := os.Stat(path)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Files = append(manifest.Files, FileRecord{Name: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size()})
	}
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	manifestFile, err := os.OpenFile(filepath.Join(temporary, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := manifestFile.Write(encoded); err != nil {
		_ = manifestFile.Close()
		return Manifest{}, err
	}
	if err := manifestFile.Sync(); err != nil {
		_ = manifestFile.Close()
		return Manifest{}, err
	}
	if err := manifestFile.Close(); err != nil {
		return Manifest{}, err
	}
	if err := syncDirectory(temporary); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(temporary, final); err != nil {
		return Manifest{}, err
	}
	if err := syncDirectory(service.root); err != nil {
		return Manifest{}, err
	}
	cleanup = false
	return manifest, nil
}

func (service *Service) List(ctx context.Context) ([]Manifest, error) {
	entries, err := os.ReadDir(service.root)
	if err != nil {
		return nil, err
	}
	manifests := make([]Manifest, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "backup-") {
			continue
		}
		manifest, err := service.Verify(ctx, entry.Name())
		if err == nil {
			manifests = append(manifests, manifest)
		}
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].CreatedAt.After(manifests[j].CreatedAt) })
	return manifests, nil
}

func (service *Service) Verify(ctx context.Context, id string) (Manifest, error) {
	if filepath.Base(id) != id || !strings.HasPrefix(id, "backup-") {
		return Manifest{}, ErrInvalid
	}
	directory := filepath.Join(service.root, id)
	encoded, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrNotFound
	}
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if json.Unmarshal(encoded, &manifest) != nil || manifest.ID != id || manifest.SchemaVersion != 1 || len(manifest.Files) != len(service.sources) {
		return Manifest{}, ErrInvalid
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, record := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if record.Name == "" || filepath.Base(record.Name) != record.Name || service.sources[record.Name] == nil {
			return Manifest{}, ErrInvalid
		}
		if _, duplicate := seen[record.Name]; duplicate {
			return Manifest{}, ErrInvalid
		}
		seen[record.Name] = struct{}{}
		path := filepath.Join(directory, record.Name)
		file, err := os.Open(path)
		if err != nil {
			return Manifest{}, ErrInvalid
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		_ = file.Close()
		if copyErr != nil || size != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
			return Manifest{}, ErrInvalid
		}
		database, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond})
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %s is not readable bbolt", ErrInvalid, record.Name)
		}
		checkErr := database.View(func(tx *bbolt.Tx) error {
			for issue := range tx.Check() {
				return issue
			}
			return nil
		})
		_ = database.Close()
		if checkErr != nil {
			return Manifest{}, fmt.Errorf("%w: %s failed bbolt check", ErrInvalid, record.Name)
		}
	}
	return manifest, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
