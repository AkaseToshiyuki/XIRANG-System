package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestMixedApplicationArchitectureFilterKeepsValidationAndFirstMapping(t *testing.T) {
	variant := func(arch, node string) domain.NativeArtifactVariant {
		return domain.NativeArtifactVariant{BuildID: "build_" + arch, Variant: arch, NodeIDs: []string{node}, Artifact: domain.NativeArtifact{URI: "file:///shared/" + arch, SHA256: strings.Repeat("a", 64), Architecture: arch, Entrypoint: "app"}}
	}
	request := SubmitRequest{ProjectID: "project", Queue: "default", Task: domain.TaskSpec{Application: "app", ArtifactVariants: []domain.NativeArtifactVariant{variant("arm64", "arm"), variant("amd64", "x86")}, Resources: domain.ResourceVector{MilliCPU: 1000, MemoryBytes: 1 << 30}, Timeout: time.Minute, Placement: domain.Placement{RequiredArchitecture: "amd64"}}}
	if err := validateSubmission(request); err != nil {
		t.Fatalf("valid mixed application rejected: %v", err)
	}
	request.Task.Placement.RequiredArchitecture = "arm64"
	if err := validateSubmission(request); err != nil {
		t.Fatal(err)
	}
	request.Task.Placement.RequiredArchitecture = "riscv64"
	if err := validateSubmission(request); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
	request.Task.Placement.RequiredArchitecture = "amd64"
	request.Task.ArtifactVariants[0].Artifact.SHA256 = "invalid"
	if err := validateSubmission(request); err == nil {
		t.Fatal("malformed nonselected variant bypassed validation")
	}
	request.Task.ArtifactVariants[0] = variant("arm64", "x86")
	if artifact, ok := artifactForNode(request.Task, "x86"); !ok || artifact.Architecture != "arm64" {
		t.Fatal("architecture filter changed ordered mapping semantics")
	}
}
