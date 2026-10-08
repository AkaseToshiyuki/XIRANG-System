package build

import "sync"

// buildLogTail drains all process output while retaining a fixed-size tail.
// The backing allocation does not grow with the build's output volume.
type buildLogTail struct {
	mu         sync.Mutex
	buffer     []byte
	next, size int
}

func newBuildLogTail(capacity int) *buildLogTail {
	return &buildLogTail{buffer: make([]byte, capacity)}
}

func (tail *buildLogTail) Write(p []byte) (int, error) {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	n := len(p)
	capacity := len(tail.buffer)
	if capacity == 0 {
		return n, nil
	}
	if n >= capacity {
		copy(tail.buffer, p[n-capacity:])
		tail.next, tail.size = 0, capacity
		return n, nil
	}
	first := copy(tail.buffer[tail.next:], p)
	copy(tail.buffer, p[first:])
	tail.next = (tail.next + n) % capacity
	tail.size = min(capacity, tail.size+n)
	return n, nil
}

func (tail *buildLogTail) String() string {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	if tail.size < len(tail.buffer) {
		return string(tail.buffer[:tail.size])
	}
	result := make([]byte, tail.size)
	n := copy(result, tail.buffer[tail.next:])
	copy(result[n:], tail.buffer[:tail.next])
	return string(result)
}
