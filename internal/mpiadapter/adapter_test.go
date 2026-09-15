package mpiadapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeRequiresExplicitVendorName(t *testing.T) {
	for _, runtime := range []string{"openmpi", "mpich"} {
		resolved, err := Runtime("/usr/local/bin/xirang-mpi-adapter-" + runtime)
		if err != nil || resolved != runtime {
			t.Fatalf("runtime %s was not recognized: %q %v", runtime, resolved, err)
		}
	}
	if _, err := Runtime("xirang-mpi-adapter-intelmpi"); err == nil {
		t.Fatal("uncertified runtime name was accepted")
	}
}

func TestValidateInvocationChecksPrivateManifest(t *testing.T) {
	workspace := t.TempDir()
	metadata := filepath.Join(workspace, ".xirang")
	if err := os.Mkdir(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(metadata, "allocation.json")
	data := []byte(`{"version":"xirang-allocation-v1","allocationId":"allocation_test","launcher":"mpich","rankStart":0,"rankCount":2,"worldSize":2,"hosts":[{"nodeId":"node-a","address":"10.0.0.1","slots":2}]}`)
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"--xirang-manifest", manifest, "--xirang-local-rank", "1", "--", "/shared/app", "input.dat"}
	if err := validateInvocation("mpich", arguments); err != nil {
		t.Fatal(err)
	}
	if err := validateInvocation("openmpi", arguments); err == nil {
		t.Fatal("manifest for another runtime was accepted")
	}
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateInvocation("mpich", arguments); err == nil {
		t.Fatal("world-readable allocation manifest was accepted")
	}
}
