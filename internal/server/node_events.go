package server

import (
	"context"
	"sync"

	"github.com/xirang-grid/xirang/internal/domain"
)

type NodeEventBroker struct {
	mu          sync.Mutex
	subscribers map[uint64]chan domain.Node
	nextID      uint64
}

func NewNodeEventBroker() *NodeEventBroker {
	return &NodeEventBroker{subscribers: make(map[uint64]chan domain.Node)}
}

func (broker *NodeEventBroker) Publish(node domain.Node) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, subscriber := range broker.subscribers {
		select {
		case subscriber <- node:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- node:
			default:
			}
		}
	}
}

func (broker *NodeEventBroker) Subscribe(ctx context.Context) <-chan domain.Node {
	updates := make(chan domain.Node, 32)
	broker.mu.Lock()
	broker.nextID++
	id := broker.nextID
	broker.subscribers[id] = updates
	broker.mu.Unlock()
	go func() {
		<-ctx.Done()
		broker.mu.Lock()
		delete(broker.subscribers, id)
		close(updates)
		broker.mu.Unlock()
	}()
	return updates
}
