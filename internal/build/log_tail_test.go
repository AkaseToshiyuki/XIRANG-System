package build

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
	"testing"
)

func TestBuildLogTailBoundedAcrossWrites(t *testing.T) {
	for _, capacity := range []int{0, 1, 13, 64 << 10} {
		tail := newBuildLogTail(capacity)
		var reference []byte
		for _, p := range [][]byte{[]byte("first step\n"), bytes.Repeat([]byte("abcd"), 100000), nil, []byte("last step\n"), []byte("error\n")} {
			n, err := tail.Write(p)
			if err != nil || n != len(p) {
				t.Fatalf("write: %d %v", n, err)
			}
			reference = append(reference, p...)
			want := reference[max(0, len(reference)-capacity):]
			if tail.String() != string(want) {
				t.Fatalf("capacity=%d: tail mismatch", capacity)
			}
			if len(tail.buffer) != capacity || cap(tail.buffer) != capacity {
				t.Fatal("retained allocation grew")
			}
		}
	}
}

func TestBuildLogTailDrainsProcessOutput(t *testing.T) {
	tail := newBuildLogTail(1024)
	command := exec.Command("sh", "-c", "i=0; while [ $i -lt 5000 ]; do printf 'stdout\\n'; printf 'stderr\\n' >&2; i=$((i+1)); done; printf 'final marker\\n'")
	command.Stdout, command.Stderr = tail, tail
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix([]byte(tail.String()), []byte("final marker\n")) {
		t.Fatal("lost output tail")
	}
	if len(tail.String()) != 1024 {
		t.Fatal("unexpected retained size")
	}
}

func TestBuildLogTailConcurrentWriters(t *testing.T) {
	tail := newBuildLogTail(128)
	var writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				_, _ = io.WriteString(tail, "build output\n")
				_ = tail.String()
			}
		}()
	}
	writers.Wait()
	if len(tail.String()) != 128 {
		t.Fatal("unexpected retained size")
	}
}
