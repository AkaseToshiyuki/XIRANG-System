package agent

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

func TestPathChangeQueuesRunAndCancelUntilCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release := make(chan struct{})
	manager := newSharedMountManager(ctx, func(ctx context.Context, _ *xirangv1.SharedMountOperation, _ []string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	op := testMountOperation()
	op.Action, op.SourceHost, op.MountPath, op.ClientAddress, op.ClientInterface = xirangv1.SharedMountOperation_ACTION_REPATH, "192.0.2.1", "/srv/share", "192.0.2.2", "eth0.100"
	if ok, reason := manager.Start(op); !ok {
		t.Fatal(reason)
	}
	var queue pathCommandQueue
	var observed []string
	apply := func(message *xirangv1.ControlMessage) error {
		observed = append(observed, message.MessageId)
		return nil
	}
	run := &xirangv1.ControlMessage{MessageId: "run", Payload: &xirangv1.ControlMessage_RunAssignment{RunAssignment: &xirangv1.RunAssignment{}}}
	stop := &xirangv1.ControlMessage{MessageId: "cancel", Payload: &xirangv1.ControlMessage_CancelAssignment{CancelAssignment: &xirangv1.CancelAssignment{}}}
	for _, message := range []*xirangv1.ControlMessage{run, stop} {
		if err := queue.dispatch(message, manager.changingPath(), apply); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.drain(manager.changingPath, apply); err != nil || len(observed) != 0 {
		t.Fatalf("command started before mount completed: %v %v", observed, err)
	}
	close(release)
	if result := <-manager.results; !result.Success {
		t.Fatal(result)
	}
	if err := queue.drain(manager.changingPath, apply); err != nil || !reflect.DeepEqual(observed, []string{"run", "cancel"}) {
		t.Fatalf("command order: %v %v", observed, err)
	}
}

func TestPathCommandQueueStopsAtAnotherPathChangeAndIsBounded(t *testing.T) {
	queue := pathCommandQueue{pending: []*xirangv1.ControlMessage{{MessageId: "next-path"}, {MessageId: "run"}}}
	changing := false
	var observed []string
	apply := func(message *xirangv1.ControlMessage) error {
		observed = append(observed, message.MessageId)
		changing = message.MessageId == "next-path"
		return nil
	}
	if err := queue.drain(func() bool { return changing }, apply); err != nil || len(observed) != 1 || len(queue.pending) != 1 {
		t.Fatalf("drained through another mount change: %v %v", observed, err)
	}
	for len(queue.pending) < 128 {
		if err := queue.dispatch(&xirangv1.ControlMessage{}, true, apply); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.dispatch(&xirangv1.ControlMessage{}, true, apply); err == nil {
		t.Fatal("unbounded pending commands")
	}
}

func TestMissingSharedMountBlocksNewWorkButPreservesRecordedAcknowledgement(t *testing.T) {
	state := commandState{Mode: "ready", Acknowledgements: []commandAcknowledgment{{MessageID: "recorded", Accepted: true}}}
	handlers := commandHandlers{mounts: &sharedMountManager{workCheck: func() error { return fmt.Errorf("shared mount unavailable") }}}
	for _, id := range []string{"new", "recorded"} {
		message := &xirangv1.ControlMessage{MessageId: id, Payload: &xirangv1.ControlMessage_RunAssignment{RunAssignment: &xirangv1.RunAssignment{}}}
		updated, ack, err := applyControlCommandWithHandlers(context.Background(), t.TempDir(), state, message, handlers)
		if err != nil || ack.Accepted != (id == "recorded") || len(updated.Acknowledgements) != 1 {
			t.Fatalf("unexpected acknowledgement for %s: %v %v", id, ack, err)
		}
	}
}
