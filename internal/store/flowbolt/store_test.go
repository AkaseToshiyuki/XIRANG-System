package flowbolt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestStoreRetainsBoundedPerNodeHistoryAndLatest(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "flows.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 1; index <= 3; index++ {
		if err := store.Append(context.Background(), domain.NodeFlowObservation{
			MessageID: string(rune('0' + index)), NodeID: "node-1", ObservedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.List(context.Background(), "node-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].MessageID != "3" || history[1].MessageID != "2" {
		t.Fatalf("unexpected retained history: %#v", history)
	}
	latest, err := store.ListLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[0].MessageID != "3" {
		t.Fatalf("unexpected latest: %#v", latest)
	}
}
