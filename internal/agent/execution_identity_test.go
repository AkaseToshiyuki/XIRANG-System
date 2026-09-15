package agent

import (
	"syscall"
	"testing"
)

func TestExecutionUIDPoolSeparatesConcurrentAssignmentsAndReusesReleasedIDs(t *testing.T) {
	pool, err := newExecutionUIDPool(200000, 200002, func(uid uint32) bool { return uid == 200001 })
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.Allocate("assignment-a")
	if err != nil || first != 200000 {
		t.Fatalf("first UID=%d err=%v", first, err)
	}
	second, err := pool.Allocate("assignment-b")
	if err != nil || second != 200002 || second == first {
		t.Fatalf("second UID=%d err=%v", second, err)
	}
	if _, err := pool.Allocate("assignment-c"); err == nil {
		t.Fatal("exhausted UID pool accepted another concurrent assignment")
	}
	pool.Release("assignment-a")
	reused, err := pool.Allocate("assignment-c")
	if err != nil || reused != first {
		t.Fatalf("released UID was not reused safely: UID=%d err=%v", reused, err)
	}
}

func TestExecutionCredentialPreservesOnlyTemplateGroups(t *testing.T) {
	template := &syscall.Credential{Uid: 997, Gid: 997, Groups: []uint32{44, 109}}
	credential, err := executionCredential(template, 200123)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Uid != 200123 || credential.Gid != 997 || len(credential.Groups) != 2 || credential.Groups[0] != 44 {
		t.Fatalf("unexpected temporary credential: %#v", credential)
	}
	credential.Groups[0] = 7
	if template.Groups[0] != 44 {
		t.Fatal("temporary credential aliases the template supplementary groups")
	}
}

func TestTemporaryUIDRangeRequiresBothCustomBounds(t *testing.T) {
	minimum, maximum, err := temporaryUIDRange(0, 0)
	if err != nil || minimum != defaultTaskUIDMin || maximum != defaultTaskUIDMax {
		t.Fatalf("default range=%d-%d err=%v", minimum, maximum, err)
	}
	if _, _, err := temporaryUIDRange(200000, 0); err == nil {
		t.Fatal("half-configured UID range was accepted")
	}
}
