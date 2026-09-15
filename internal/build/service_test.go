package build_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/domain"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

var adminPrincipal = domain.Principal{Subject: "admin", DisplayName: "Administrator", Role: "admin", AuthMethod: "api_token"}

type fakeRunner struct{}

type blockingRunner struct{ fakeRunner }

func (blockingRunner) Run(ctx context.Context, _ string, _ domain.BuildRecipe) (string, error) {
	<-ctx.Done()
	return "cancel observed\n", ctx.Err()
}

func (fakeRunner) Worker() domain.BuildWorker {
	return domain.BuildWorker{ID: "test-worker", Architecture: runtime.GOARCH, Toolchains: []string{"go"}, Isolation: "test", Ready: true, SupportsCancellation: true}
}

func (fakeRunner) Run(_ context.Context, workspace string, _ domain.BuildRecipe) (string, error) {
	data, err := os.ReadFile("/bin/true")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(workspace, "bin"), 0o750); err != nil {
		return "", err
	}
	return "build complete\n", os.WriteFile(filepath.Join(workspace, "bin", "app"), data, 0o750)
}

func TestBuildProducesSignedArtifactAndSPDX(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "sources")
	project := filepath.Join(sourceRoot, "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("source\n")
	if err := os.WriteFile(filepath.Join(project, "hello.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("hello.txt\x00"))
	_, _ = hash.Write(content)
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := buildmanager.New(store, buildmanager.Config{
		SourceRoot: sourceRoot, WorkspaceRoot: filepath.Join(root, "work"),
		ArtifactRoot: filepath.Join(root, "artifacts"), ArtifactBaseURL: "https://artifacts.example/xirang",
		SigningKeyPath: filepath.Join(root, "keys", "build.key"), Runner: fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "work")); err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("workspace root must be searchable but not listable: info=%v err=%v", info, err)
	}
	submitted, err := service.Submit(context.Background(), domain.BuildRecipe{
		Name: "example", SourcePath: "project",
		Architecture: runtime.GOARCH, Command: []string{"go", "build", "./..."},
		OutputPath: "bin/app", Toolchain: "go", MilliCPU: 1000, MemoryBytes: 256 << 20, Timeout: time.Minute,
	}, adminPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.Build
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, err = service.Get(context.Background(), submitted.ID)
		if err == nil && result.Status != domain.BuildStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Status != domain.BuildStatusSucceeded || result.Artifact.SBOMURI == "" ||
		result.Artifact.ABI == "" || result.LogTail != "build complete\n" {
		t.Fatalf("unexpected build result: %#v", result)
	}
	if result.Recipe.SourceSHA256 != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatalf("source tree checksum was not calculated by the controller: %q", result.Recipe.SourceSHA256)
	}
	digest, _ := hex.DecodeString(result.Artifact.SHA256)
	signature, _ := base64.StdEncoding.DecodeString(result.Artifact.Signature)
	publicKey, _ := base64.StdEncoding.DecodeString(result.Artifact.PublicKey)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), digest, signature) {
		t.Fatal("artifact signature did not verify")
	}
	oldKey := service.SigningKey()
	newKey, err := service.RotateSigningKey(context.Background(), domain.Principal{
		Subject: "root", AuthMethod: "bootstrap", Role: "admin", Permissions: []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if newKey.Fingerprint == oldKey.Fingerprint || newKey.PublicKey == oldKey.PublicKey {
		t.Fatal("signing-key rotation did not replace the trust root")
	}
	events, err := store.ListAuditEvents(context.Background(), 10)
	if err != nil || len(events) != 1 || events[0].Action != "build.signing_key.rotated" {
		t.Fatalf("unexpected signing-key audit trail: events=%#v err=%v", events, err)
	}
	builds, err := service.List(context.Background(), 10)
	if err != nil || len(builds) != 1 || builds[0].ID != result.ID {
		t.Fatalf("build was not durable: builds=%#v err=%v", builds, err)
	}
}

func TestApplicationBuildPublishesOnlyToSharedRouteAndCanRemapNodes(t *testing.T) {
	root := t.TempDir()
	sourceRoot, project := filepath.Join(root, "sources"), filepath.Join(root, "sources", "gromacs")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "CMakeLists.txt"), []byte("project(gromacs)\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sharedRoot := filepath.Join(root, "shared")
	service, err := buildmanager.New(store, buildmanager.Config{
		SourceRoot: sourceRoot, WorkspaceRoot: filepath.Join(root, "work"), ArtifactRoot: filepath.Join(root, "artifacts"),
		SigningKeyPath: filepath.Join(root, "build.key"), Runner: fakeRunner{},
		ResolveSharedPath: func(_ context.Context, spaceID, relative string) (string, error) {
			if spaceID != "share_default" {
				return "", errors.New("unknown space")
			}
			return filepath.Join(sharedRoot, filepath.FromSlash(relative)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := service.Submit(context.Background(), domain.BuildRecipe{
		Name: "gromacs cuda", Application: "gromacs", Variant: "amd64-nvidia", RuntimeNodeIDs: []string{"node-a"}, RuntimeEntrypoint: "gmx",
		SourcePath: "gromacs", Architecture: runtime.GOARCH, Steps: [][]string{{"cmake", "-S", ".", "-B", "build"}, {"cmake", "--build", "build"}},
		OutputPath: "bin/app", OutputSpaceID: "share_default", PublishPath: "builds/gromacs/amd64-nvidia/gmx",
		Toolchain: "go", MilliCPU: 1000, MemoryBytes: 256 << 20, Timeout: time.Minute,
	}, adminPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	var completed domain.Build
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		completed, err = service.Get(context.Background(), submitted.ID)
		if err == nil && completed.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := filepath.Join(sharedRoot, "builds/gromacs/amd64-nvidia/gmx")
	if completed.Status != domain.BuildStatusSucceeded || completed.Artifact.URI != "file://"+want {
		t.Fatalf("unexpected shared application artifact: %#v", completed)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", completed.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("controller artifact duplicate still exists: %v", err)
	}
	variants, err := service.ResolveApplication(context.Background(), "gromacs")
	if err != nil || len(variants) != 1 || variants[0].NodeIDs[0] != "node-a" {
		t.Fatalf("application resolution failed: variants=%#v err=%v", variants, err)
	}
	updated, err := service.UpdateRuntimeTargets(context.Background(), completed.ID, []string{"node-b", "node-a"}, adminPrincipal)
	if err != nil || len(updated.Artifact.RuntimeNodeIDs) != 2 || updated.Artifact.RuntimeNodeIDs[0] != "node-a" {
		t.Fatalf("runtime target remap failed: build=%#v err=%v", updated, err)
	}
}

func TestRetentionCanPreserveSuccessfulBuildsPermanently(t *testing.T) {
	root := t.TempDir()
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifactRoot := filepath.Join(root, "artifacts")
	service, err := buildmanager.New(store, buildmanager.Config{
		SourceRoot: filepath.Join(root, "sources"), WorkspaceRoot: filepath.Join(root, "work"), ArtifactRoot: artifactRoot,
		SigningKeyPath: filepath.Join(root, "build.key"), Runner: fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	for _, build := range []domain.Build{
		{ID: "build_succeeded", Status: domain.BuildStatusSucceeded, CreatedAt: old, StartedAt: old, FinishedAt: &old},
		{ID: "build_failed", Status: domain.BuildStatusFailed, CreatedAt: old.Add(time.Minute), StartedAt: old, FinishedAt: &old},
	} {
		if err := store.PutBuild(context.Background(), build); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(artifactRoot, build.ID)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "app"), []byte(build.ID), 0o550); err != nil {
			t.Fatal(err)
		}
	}
	actor := domain.Principal{Subject: "admin", AuthMethod: "local", Role: "admin"}
	policy, err := service.UpdateRetentionPolicy(context.Background(), domain.BuildRetentionPolicy{
		MaximumRecords: 1, MaximumAge: 24 * time.Hour, PreserveSuccessfulBuilds: true,
	}, actor)
	if err != nil || !policy.PreserveSuccessfulBuilds {
		t.Fatalf("permanent preservation policy failed: policy=%#v err=%v", policy, err)
	}
	result, err := service.Cleanup(context.Background(), actor)
	if err != nil || result.DeletedRecords != 1 {
		t.Fatalf("unexpected cleanup result: result=%#v err=%v", result, err)
	}
	if _, err := service.Get(context.Background(), "build_succeeded"); err != nil {
		t.Fatalf("successful build record was cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifactRoot, "build_succeeded", "app")); err != nil {
		t.Fatalf("successful build artifact was cleaned: %v", err)
	}
	if _, err := service.Get(context.Background(), "build_failed"); !errors.Is(err, buildmanager.ErrNotFound) {
		t.Fatalf("expired failed build was not cleaned: %v", err)
	}
}

func TestBuildCancellationAndRetentionLifecycle(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "sources")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "project"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "project", "input.txt"), []byte("input"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := buildmanager.New(store, buildmanager.Config{
		SourceRoot: sourceRoot, WorkspaceRoot: filepath.Join(root, "work"), ArtifactRoot: filepath.Join(root, "artifacts"),
		SigningKeyPath: filepath.Join(root, "keys", "build.key"), Runner: blockingRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{Subject: "admin", AuthMethod: "api_token", Role: "admin"}
	submitted, err := service.Submit(context.Background(), domain.BuildRecipe{
		Name: "cancel-me", SourcePath: "project", Architecture: runtime.GOARCH, Command: []string{"go", "build"},
		OutputPath: "bin/app", Toolchain: "go", MilliCPU: 1000, MemoryBytes: 1 << 20, Timeout: time.Minute,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Recipe.SourceSHA256 == "" {
		t.Fatal("automatic source checksum is empty")
	}
	intruder := domain.Principal{UserID: "user-2", Subject: "other", Role: "builder", AuthMethod: "api_token"}
	if _, err := service.Cancel(context.Background(), submitted.ID, intruder); !errors.Is(err, buildmanager.ErrForbidden) {
		t.Fatalf("another builder canceled an owned build: %v", err)
	}
	if _, err := service.UpdateRetentionPolicy(context.Background(), domain.BuildRetentionPolicy{
		MaximumRecords: 10, MaximumAge: 24 * time.Hour,
	}, intruder); !errors.Is(err, buildmanager.ErrForbidden) {
		t.Fatalf("non-administrator changed global retention: %v", err)
	}
	if _, err := service.Cancel(context.Background(), submitted.ID, actor); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := service.Get(context.Background(), submitted.ID)
		if getErr == nil && current.Status == domain.BuildStatusCanceled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	canceled, err := service.Get(context.Background(), submitted.ID)
	if err != nil || canceled.Status != domain.BuildStatusCanceled {
		t.Fatalf("build not canceled: %#v err=%v", canceled, err)
	}
	policy, err := service.UpdateRetentionPolicy(context.Background(), domain.BuildRetentionPolicy{MaximumRecords: 10, MaximumAge: 24 * time.Hour}, actor)
	if err != nil || policy.MaximumRecords != 10 {
		t.Fatalf("retention update failed: %#v err=%v", policy, err)
	}
	result, err := service.Delete(context.Background(), submitted.ID, actor)
	if err != nil || result.DeletedRecords != 1 {
		t.Fatalf("terminal build deletion failed: %#v err=%v", result, err)
	}
	if _, err := service.Get(context.Background(), submitted.ID); !errors.Is(err, buildmanager.ErrNotFound) {
		t.Fatalf("deleted build lookup error = %v", err)
	}
}

func TestBuildReadsAndPublishesInsideSelectedSharedSpaces(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	project := filepath.Join(shared, "source", "project")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("source\n")
	if err := os.WriteFile(filepath.Join(project, "hello.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("hello.txt\x00"))
	_, _ = hash.Write(content)
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := buildmanager.New(store, buildmanager.Config{
		SourceRoot: filepath.Join(root, "legacy-source"), WorkspaceRoot: filepath.Join(root, "work"), ArtifactRoot: filepath.Join(root, "artifacts"),
		SigningKeyPath: filepath.Join(root, "keys", "build.key"), Runner: fakeRunner{},
		ResolveSharedPath: func(_ context.Context, spaceID, relative string) (string, error) {
			return filepath.Join(shared, spaceID, filepath.FromSlash(relative)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := service.Submit(context.Background(), domain.BuildRecipe{
		Name: "shared", SourceSpaceID: "source", SourcePath: "project", SourceSHA256: hex.EncodeToString(hash.Sum(nil)),
		OutputSpaceID: "published", PublishPath: "releases/app", OutputPath: "bin/app", Architecture: runtime.GOARCH, BuilderArchitecture: runtime.GOARCH,
		WorkerID: "test-worker", Command: []string{"go", "build"}, Toolchain: "go", MilliCPU: 1000, MemoryBytes: 1 << 30, Timeout: time.Minute,
	}, adminPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, getErr := service.Get(context.Background(), submitted.ID)
		if getErr == nil && result.Status != domain.BuildStatusRunning {
			if result.Status != domain.BuildStatusSucceeded {
				t.Fatalf("build failed: %#v", result)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info, err := os.Stat(filepath.Join(shared, "published", "releases", "app")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published output missing: info=%v err=%v", info, err)
	}
}
