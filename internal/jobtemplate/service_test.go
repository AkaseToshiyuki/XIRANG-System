package jobtemplate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestTemplateLifecyclePersistsAndRejectsStaleRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xirang.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := jobtemplate.New(store)
	actor := domain.Principal{Subject: "cluster-admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	created, err := service.Create(context.Background(), "CPU smoke", "Run one portable smoke task.", validScript("amd64"), actor)
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.CreatedBy != actor.Subject || created.Script[len(created.Script)-1] != '\n' {
		t.Fatalf("unexpected created template: %#v", created)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service = jobtemplate.New(store)
	listed, err := service.List(context.Background())
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("template was not persisted: templates=%#v err=%v", listed, err)
	}
	updated, err := service.Update(context.Background(), created.ID, created.Revision, "CPU smoke v2", "Updated", validScript("arm64"), actor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Name != "CPU smoke v2" {
		t.Fatalf("unexpected updated template: %#v", updated)
	}
	if _, err := service.Update(context.Background(), created.ID, created.Revision, "stale", "", validScript("amd64"), actor); !errors.Is(err, jobtemplate.ErrConflict) {
		t.Fatalf("stale revision was accepted: %v", err)
	}
	if _, err := service.Create(context.Background(), "cpu SMOKE V2", "", validScript("amd64"), actor); !errors.Is(err, jobtemplate.ErrConflict) {
		t.Fatalf("duplicate template name was accepted: %v", err)
	}
	if _, err := service.Create(context.Background(), "invalid", "", "XIRANG 1\ncpu = 1", actor); !errors.Is(err, jobtemplate.ErrInvalid) {
		t.Fatalf("invalid script was accepted: %v", err)
	}
	if err := service.Delete(context.Background(), created.ID, updated.Revision, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), created.ID); !errors.Is(err, jobtemplate.ErrNotFound) {
		t.Fatalf("deleted template remained readable: %v", err)
	}
	events, err := store.ListAuditEvents(context.Background(), 10)
	if err != nil || len(events) != 3 || events[0].Action != "job_template.deleted" ||
		events[1].Action != "job_template.updated" || events[2].Action != "job_template.created" {
		t.Fatalf("unexpected template audit trail: events=%#v err=%v", events, err)
	}
}

func TestEnsureBuiltinsPublishesTemplatesOnlyOnce(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(filepath.Join(artifactRoot, "v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cpu-pi", "cuda-smoke", "arm64-acceptance"} {
		if err := os.WriteFile(filepath.Join(artifactRoot, "v1", name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := jobtemplate.New(store)
	marker := filepath.Join(root, "builtin-job-templates-v1")
	if err := jobtemplate.EnsureBuiltins(context.Background(), service, marker, artifactRoot); err != nil {
		t.Fatal(err)
	}
	templates, err := service.List(context.Background())
	if err != nil || len(templates) != 3 {
		t.Fatalf("unexpected built-in templates: %#v err=%v", templates, err)
	}
	foundCUDA := false
	for _, template := range templates {
		if template.Name == "CUDA smoke test" && template.Script != "" {
			foundCUDA = true
		}
	}
	if !foundCUDA {
		t.Fatalf("CUDA built-in template missing: %#v", templates)
	}
	if err := jobtemplate.EnsureBuiltins(context.Background(), service, marker, filepath.Join(root, "missing")); err != nil {
		t.Fatalf("existing marker should make initialization idempotent: %v", err)
	}
	templates, err = service.List(context.Background())
	if err != nil || len(templates) != 3 {
		t.Fatalf("built-ins were duplicated: %#v err=%v", templates, err)
	}
}

// TestEnsureBuiltinsRelabelsLegacyChineseTemplates covers the clusters that
// already exist. They seeded the built-ins under Simplified Chinese names and
// wrote the marker that stops seeding, so without this pass the console would
// keep rendering a Chinese name and description under an English heading
// forever.
func TestEnsureBuiltinsRelabelsLegacyChineseTemplates(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(filepath.Join(artifactRoot, "v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cpu-pi", "cuda-smoke", "arm64-acceptance"} {
		if err := os.WriteFile(filepath.Join(artifactRoot, "v1", name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := boltstore.Open(filepath.Join(root, "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := jobtemplate.New(store)
	actor := domain.Principal{Subject: "admin", Role: "admin", AuthMethod: "token"}

	// Exactly what a pre-1.8.16-dev cluster holds.
	legacy, err := service.Create(context.Background(), "CUDA 基础测试",
		"申请 1 张 NVIDIA GPU，使用 nvcc 编译并运行 CUDA 向量加法，同时验证设备枚举、内存传输和核函数结果。",
		validScript("amd64"), actor)
	if err != nil {
		t.Fatal(err)
	}
	// An administrator's own template, and a built-in they have since renamed:
	// neither is XIRANG's to relabel.
	mine, err := service.Create(context.Background(), "夜间回归", "本地脚本", validScript("amd64"), actor)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := service.Create(context.Background(), "ARM64 节点验收", "我们自己的验收说明", validScript("arm64"), actor)
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "builtin-job-templates-v1")
	if err := os.WriteFile(marker, []byte("seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jobtemplate.EnsureBuiltins(context.Background(), service, marker, artifactRoot); err != nil {
		t.Fatal(err)
	}

	renamed, err := service.Get(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "CUDA smoke test" {
		t.Fatalf("the shipped built-in kept its Chinese name: %q", renamed.Name)
	}
	if strings.ContainsAny(renamed.Description, "申请编译") {
		t.Fatalf("the shipped built-in kept its Chinese description: %q", renamed.Description)
	}
	if renamed.Script != legacy.Script {
		t.Fatal("relabelling rewrote the template script")
	}
	for _, untouched := range []domain.JobTemplate{mine, edited} {
		after, err := service.Get(context.Background(), untouched.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Name != untouched.Name || after.Description != untouched.Description || after.Revision != untouched.Revision {
			t.Fatalf("an administrator's template was rewritten: %#v", after)
		}
	}

	// Running again must not touch anything: the pass carries no marker of its
	// own and relies on no row matching a second time.
	if err := jobtemplate.EnsureBuiltins(context.Background(), service, marker, artifactRoot); err != nil {
		t.Fatal(err)
	}
	stable, err := service.Get(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Revision != renamed.Revision {
		t.Fatalf("a second start bumped the template again: %d -> %d", renamed.Revision, stable.Revision)
	}
}

func validScript(architecture string) string {
	return `XIRANG 1
project = smoke
queue = default
artifact = https://artifacts.example/smoke
sha256 = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
architecture = ` + architecture + `
entrypoint = smoke
cpu = 1
memory = 128M
`
}
