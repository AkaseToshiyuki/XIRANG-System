package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testMountOperation() *xirangv1.SharedMountOperation {
	return &xirangv1.SharedMountOperation{OperationId: "mount_test", SpaceId: "share", Action: xirangv1.SharedMountOperation_ACTION_EXPORT, SourcePath: "/srv/share", ExpiresAt: timestamppb.New(time.Now().Add(20 * time.Second))}
}

func TestSharedMountManagerIsBoundedAsyncAndDeduplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release := make(chan struct{})
	var runs atomic.Int32
	manager := newSharedMountManager(ctx, func(ctx context.Context, _ *xirangv1.SharedMountOperation, args []string) error {
		runs.Add(1)
		if !reflect.DeepEqual(args, []string{"export", "share", "/srv/share"}) {
			return errors.New("wrong arguments")
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	operation := testMountOperation()
	if ok, reason := manager.Start(operation); !ok {
		t.Fatal(reason)
	}
	if ok, reason := manager.Start(operation); !ok {
		t.Fatal(reason)
	}
	changed := proto.Clone(operation).(*xirangv1.SharedMountOperation)
	changed.SourcePath = "/srv/other"
	if ok, _ := manager.Start(changed); ok {
		t.Fatal("changed duplicate accepted")
	}
	changed.OperationId = "mount_second"
	if ok, _ := manager.Start(changed); ok {
		t.Fatal("concurrent mutation accepted")
	}
	select {
	case <-manager.results:
		t.Fatal("acceptance was treated as completion")
	default:
	}
	close(release)
	result := <-manager.results
	if !result.Success || runs.Load() != 1 {
		t.Fatalf("result %v runs %d", result, runs.Load())
	}
	if ok, reason := manager.Start(operation); !ok {
		t.Fatal(reason)
	}
	if replay := <-manager.results; !proto.Equal(replay, result) || runs.Load() != 1 {
		t.Fatal("duplicate re-executed")
	}
}

func TestSharedMountRejectsDisabledExpiredAndCancelsDisconnect(t *testing.T) {
	var disabled *sharedMountManager
	if ok, _ := disabled.Start(testMountOperation()); ok {
		t.Fatal("disabled execution")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	manager := newSharedMountManager(ctx, func(ctx context.Context, _ *xirangv1.SharedMountOperation, _ []string) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	expired := testMountOperation()
	expired.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
	if ok, _ := manager.Start(expired); ok {
		t.Fatal("expired execution")
	}
	if ok, reason := manager.Start(testMountOperation()); !ok {
		t.Fatal(reason)
	}
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel execution")
	}
	if ok, _ := manager.Start(testMountOperation()); ok {
		t.Fatal("disconnected manager accepted work")
	}
}

func TestSharedMountHostUnitArgumentsAreFixedAndOutputBounded(t *testing.T) {
	operation := testMountOperation()
	args := sharedMountUnitArguments("/usr/local/bin/xirang-agent", operation, []string{"export", "share", "/srv/share"})
	joined := strings.Join(args, "\n")
	for _, required := range []string{"--unit=xirang-shared-mount_test", "--wait", "--pipe", "--collect", "--service-type=exec", "--property=RuntimeMaxSec=30s", "--property=KillMode=control-group", "__xirang-shared-mount"} {
		if !strings.Contains(joined, required) {
			t.Fatal("missing host execution constraint", required)
		}
	}
	if !reflect.DeepEqual(args[len(args)-3:], []string{"export", "share", "/srv/share"}) {
		t.Fatal("changed command arguments")
	}
	if strings.Contains(joined, "--scope") || strings.Contains(joined, "sh -c") {
		t.Fatal("host namespace or shell boundary changed")
	}
	var output mountOutput
	if n, err := output.Write([]byte(strings.Repeat("x", 2048))); err != nil || n != 2048 || len(output.String()) != 1024 {
		t.Fatal("unbounded output")
	}
	if err := RunHostSharedMount([]string{"0", "file-delete", "share", "/srv/share"}); err == nil {
		t.Fatal("invalid internal invocation accepted")
	}
}

func TestSharedMountCommandRequiresMatchingIDAndDoesNotPersistAcceptance(t *testing.T) {
	operation := testMountOperation()
	message := &xirangv1.ControlMessage{MessageId: "wrong", Payload: &xirangv1.ControlMessage_SharedMountOperation{SharedMountOperation: operation}}
	directory := t.TempDir()
	_, ack, err := applyControlCommandWithHandlers(context.Background(), directory, commandState{Mode: "ready"}, message, commandHandlers{})
	if err != nil || ack.Accepted {
		t.Fatal("mismatched operation accepted")
	}
	message.MessageId = operation.OperationId
	_, ack, err = applyControlCommandWithHandlers(context.Background(), directory, commandState{Mode: "ready"}, message, commandHandlers{})
	if err != nil || ack.Accepted {
		t.Fatal("disabled capability accepted")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("ephemeral acknowledgement entered durable command ledger")
	}
}

func TestSharedMountLedgerRetiresOnlyCompletedExpiredOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager := newSharedMountManager(ctx, func(context.Context, *xirangv1.SharedMountOperation, []string) error { return nil })
	expired := testMountOperation()
	expired.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
	manager.records["completed"] = &mountRecord{operation: expired, result: &xirangv1.SharedMountResult{Success: true}}
	manager.records["running"] = &mountRecord{operation: expired}
	manager.records["recent"] = &mountRecord{operation: testMountOperation(), result: &xirangv1.SharedMountResult{Success: true}}
	if ok, reason := manager.Start(testMountOperation()); !ok {
		t.Fatal(reason)
	}
	<-manager.results
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.records["completed"] != nil || manager.records["running"] == nil || manager.records["recent"] == nil {
		t.Fatal("ledger retention lost replay/running boundary")
	}
}
