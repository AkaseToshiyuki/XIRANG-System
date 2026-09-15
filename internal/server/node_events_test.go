package server

import (
	"context"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestNodeEventBrokerPublishesAndBoundsSlowSubscriber(t *testing.T) {
	broker := NewNodeEventBroker()
	ctx, cancel := context.WithCancel(context.Background())
	updates := broker.Subscribe(ctx)
	broker.Publish(domain.Node{ID: "node-first", Revision: 1})
	if node := <-updates; node.ID != "node-first" {
		t.Fatalf("unexpected first update: %#v", node)
	}
	for revision := uint64(2); revision <= 100; revision++ {
		broker.Publish(domain.Node{ID: "node-latest", Revision: revision})
	}
	var latest domain.Node
	for len(updates) > 0 {
		latest = <-updates
	}
	if latest.Revision != 100 {
		t.Fatalf("slow subscriber did not retain latest update: %#v", latest)
	}
	cancel()
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("subscription remained open after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription cancellation timed out")
	}
}
