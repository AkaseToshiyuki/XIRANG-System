package clusterops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestAgentRunnerNeverFallsBackToSSHForMountMutations(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "ssh-called")
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte("#!/bin/sh\nprintf called > '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	runner := AgentRunner{}
	node := domain.Node{ID: "legacy", SSHUser: "operator", ManagementAddress: "127.0.0.1"}
	for _, args := range [][]string{{"prepare", "share", "/srv/share"}, {"export", "share", "/srv/share"}, {"unexport", "share", "/srv/share"}, {"mount", "share", "source", "/srv/share", "/mnt/share"}, {"unmount", "share", "/mnt/share"}, {"policy-authorize", "share", "/srv/share"}} {
		if _, err := runner.Run(context.Background(), node, args...); err == nil {
			t.Fatal("mutation succeeded without an Agent", args[0])
		}
	}
	if err := runner.RunStream(context.Background(), node, nil, io.Discard, "export", "share", "/srv/share"); err == nil {
		t.Fatal("streaming mount mutation accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("mutation invoked SSH fallback")
	}
	if _, err := runner.Run(context.Background(), node, "stats", "share", "/srv/share"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("file statistics did not retain SSH path")
	}
}
