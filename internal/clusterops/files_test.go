package clusterops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type fileRunner struct {
	files   []domain.SharedFile
	uploads bytes.Buffer
	actions [][]string
}

func (runner *fileRunner) Run(_ context.Context, _ domain.Node, arguments ...string) ([]byte, error) {
	runner.actions = append(runner.actions, append([]string(nil), arguments...))
	if arguments[0] == "file-list" {
		return json.Marshal(runner.files)
	}
	return nil, nil
}

func (runner *fileRunner) RunStream(_ context.Context, _ domain.Node, input io.Reader, output io.Writer, arguments ...string) error {
	runner.actions = append(runner.actions, append([]string(nil), arguments...))
	switch arguments[0] {
	case "file-write":
		_, err := io.Copy(&runner.uploads, input)
		return err
	case "file-read":
		_, err := io.WriteString(output, "test payload")
		return err
	default:
		return nil
	}
}

func TestSharedFileOwnershipFiltersAndProtectsRegularUsers(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share": {
			ID: "share", SourceNodeID: "storage", SourcePath: "/srv/share",
			FileOwners: map[string]domain.SharedFileOwner{
				"mine.txt":  {ID: "user-a", Name: "Alice"},
				"other.txt": {ID: "user-b", Name: "Bob"},
			},
		},
	}}
	runner := &fileRunner{files: []domain.SharedFile{
		{Path: "mine.txt", Name: "mine.txt", ModifiedAt: time.Now()},
		{Path: "other.txt", Name: "other.txt", ModifiedAt: time.Now()},
		{Path: "untracked.txt", Name: "untracked.txt", Owner: "xirang-task", ModifiedAt: time.Now()},
	}}
	service := New(repository, memoryNodes{{ID: "storage"}}, runner)
	alice := domain.Principal{UserID: "user-a", Subject: "alice", DisplayName: "Alice", Role: "user"}
	service.storeCapacity("share", capacityObservation{
		usedBytes: 100, availableBytes: 900, totalBytes: 1000, observedAt: service.now(),
	})

	files, err := service.ListFiles(context.Background(), "share", "", alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "mine.txt" || !files[0].OwnedByUser || files[0].Owner != "Alice" {
		t.Fatalf("unexpected user-visible files: %#v", files)
	}
	var downloaded bytes.Buffer
	if err := service.DownloadFile(context.Background(), "share", "mine.txt", alice, &downloaded); err != nil || downloaded.String() != "test payload" {
		t.Fatalf("owned download failed: %v payload=%q", err, downloaded.String())
	}
	if err := service.DownloadFile(context.Background(), "share", "other.txt", alice, io.Discard); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other user's file must be forbidden, got %v", err)
	}
	if err := service.UploadFile(context.Background(), "share", "new.txt", alice, bytes.NewBufferString("new")); err != nil {
		t.Fatal(err)
	}
	if runner.uploads.String() != "new" || repository.spaces["share"].FileOwners["new.txt"].ID != "user-a" {
		t.Fatalf("upload ownership was not persisted: %#v", repository.spaces["share"].FileOwners)
	}
	if _, cached := service.cachedCapacity("share"); cached {
		t.Fatal("upload must invalidate cached directory usage")
	}
	if mode := runner.actions[len(runner.actions)-1][4]; mode != "create" {
		t.Fatalf("new regular-user uploads must not replace untracked files, got mode %q", mode)
	}
	if err := service.UploadFile(context.Background(), "share", "mine.txt", alice, bytes.NewBufferString("replace")); err != nil {
		t.Fatal(err)
	}
	if mode := runner.actions[len(runner.actions)-1][4]; mode != "replace" {
		t.Fatalf("owned uploads should use replace mode, got %q", mode)
	}
	if err := service.DeleteFile(context.Background(), "share", "mine.txt", alice); err != nil {
		t.Fatal(err)
	}
	if action := runner.actions[len(runner.actions)-1]; action[4] != "owned-recursive" || len(action) != 6 {
		t.Fatalf("regular-user deletion must use a verified ownership manifest, got %#v", action)
	}
}

func TestSharedFileUploadUsesAggregateUserQuota(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share-a": {ID: "share-a", SourceNodeID: "storage", SourcePath: "/srv/a", FileOwners: map[string]domain.SharedFileOwner{
			"old.bin": {ID: "user-a", Name: "Alice", SizeBytes: 6},
		}},
		"share-b": {ID: "share-b", SourceNodeID: "storage", SourcePath: "/srv/b", FileOwners: map[string]domain.SharedFileOwner{
			"other.bin": {ID: "user-a", Name: "Alice", SizeBytes: 3},
		}},
	}}
	runner := &fileRunner{}
	service := New(repository, memoryNodes{{ID: "storage"}}, runner)
	service.EnableStorageQuotas(func(context.Context, domain.Principal) (int64, error) { return 10, nil })
	alice := domain.Principal{UserID: "user-a", Subject: "alice", Role: "user"}
	if err := service.UploadFile(context.Background(), "share-a", "old.bin", alice, bytes.NewBufferString("1234")); err != nil {
		t.Fatal(err)
	}
	action := runner.actions[len(runner.actions)-1]
	if action[5] != "7" {
		t.Fatalf("replacement limit should reclaim the previous 6 bytes, got %#v", action)
	}
	if owner := repository.spaces["share-a"].FileOwners["old.bin"]; owner.SizeBytes != 4 {
		t.Fatalf("uploaded byte count was not persisted: %#v", owner)
	}
}

func TestBuildPathAuthorizationUsesFileOwnership(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{"share": {
		ID: "share", FileOwners: map[string]domain.SharedFileOwner{
			"alice": {ID: "user-a", Name: "Alice"}, "alice/source": {ID: "user-a", Name: "Alice"},
			"bob": {ID: "user-b", Name: "Bob"},
		},
	}}}
	service := New(repository, memoryNodes{}, &fileRunner{})
	alice := domain.Principal{UserID: "user-a", Subject: "alice", Role: "builder"}
	if err := service.AuthorizeSharedPath(context.Background(), "share", "alice/source", alice, false); err != nil {
		t.Fatalf("owned build source was rejected: %v", err)
	}
	if err := service.AuthorizeSharedPath(context.Background(), "share", "alice/output", alice, true); err != nil {
		t.Fatalf("output below owned directory was rejected: %v", err)
	}
	if err := service.AuthorizeSharedPath(context.Background(), "share", "bob", alice, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign build source authorization = %v", err)
	}
	if err := service.AuthorizeSharedPath(context.Background(), "share", "bob/output", alice, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign build output authorization = %v", err)
	}
}

func TestSharedFileDirectoryIsolationAndOwnedRecursiveDelete(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share": {
			ID: "share", SourceNodeID: "storage", SourcePath: "/srv/share",
			FileOwners: map[string]domain.SharedFileOwner{
				"alice": {ID: "user-a", Name: "Alice"}, "alice/input": {ID: "user-a", Name: "Alice"},
				"bob": {ID: "user-b", Name: "Bob"}, "bob/private": {ID: "user-b", Name: "Bob"},
			},
		},
	}}
	runner := &fileRunner{}
	service := New(repository, memoryNodes{{ID: "storage"}}, runner)
	alice := domain.Principal{UserID: "user-a", Subject: "alice", Role: "user"}
	if _, err := service.ListFiles(context.Background(), "share", "bob", alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user probed another owner's directory: %v", err)
	}
	if err := service.DeleteFile(context.Background(), "share", "alice", alice); err != nil {
		t.Fatal(err)
	}
	action := runner.actions[len(runner.actions)-1]
	manifestJSON, err := base64.RawURLEncoding.DecodeString(action[5])
	if err != nil {
		t.Fatal(err)
	}
	var manifest []string
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 || manifest[0] != "alice" || manifest[1] != "alice/input" {
		t.Fatalf("unexpected ownership manifest: %#v", manifest)
	}
	if _, exists := repository.spaces["share"].FileOwners["alice/input"]; exists {
		t.Fatal("deleted subtree ownership metadata remains")
	}
}

func TestOwnedSubtreeManifestRejectsMixedOwnership(t *testing.T) {
	space := domain.SharedSpace{FileOwners: map[string]domain.SharedFileOwner{
		"project": {ID: "user-a"}, "project/admin.txt": {ID: "admin"},
	}}
	if _, err := ownedSubtreeManifest(space, "project", "user-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mixed-ownership directory was accepted: %v", err)
	}
}

func TestCleanRelativePathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../secret", "dir/../../secret", "/absolute"} {
		if _, err := cleanRelativePath(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q must be rejected, got %v", value, err)
		}
	}
}
