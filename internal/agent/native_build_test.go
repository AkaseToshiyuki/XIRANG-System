package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeBuildPrivateStagingAndAtomicOutput(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	local := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(filepath.Join(shared, "src"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "src", "main.c"), []byte("int main(void){return 0;}\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := copyNativeBuildTree(shared, local); err != nil {
		t.Fatal(err)
	}
	sharedDigest, err := hashNativeBuildTree(shared)
	if err != nil {
		t.Fatal(err)
	}
	localDigest, err := hashNativeBuildTree(local)
	if err != nil || localDigest != sharedDigest {
		t.Fatalf("private staging digest mismatch: %q %q %v", sharedDigest, localDigest, err)
	}
	output := filepath.Join(local, "bin", "app")
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("artifact"), 0o700); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(shared, "bin", "app")
	digest, err := publishNativeBuildOutput(local, shared, "bin/app")
	if err != nil || !nativeBuildSHA256(digest) {
		t.Fatalf("publish failed: digest=%q err=%v", digest, err)
	}
	data, err := os.ReadFile(published)
	if err != nil || string(data) != "artifact" {
		t.Fatalf("unexpected published output: %q %v", data, err)
	}
}

func TestNativeBuildStagingRejectsSymlink(t *testing.T) {
	shared := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(shared, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := copyNativeBuildTree(shared, filepath.Join(t.TempDir(), "local")); err == nil {
		t.Fatal("symbolic link was accepted")
	}
}

func TestNativeBuildOutputRejectsLeafAndParentLinks(t *testing.T) {
	local, shared, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "app"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "app"), filepath.Join(local, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(local, "parent")); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"leaf", "parent/app"} {
		if digest, err := publishNativeBuildOutput(local, shared, output); err == nil || digest != "" {
			t.Fatalf("published linked output: %s %v", digest, err)
		}
		if _, err := os.Stat(filepath.Join(shared, output)); !os.IsNotExist(err) {
			t.Fatalf("unsafe output created: %v", err)
		}
	}
}
