package agent

import (
	"errors"
	"fmt"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

const (
	defaultTaskUIDMin      uint32 = 200000
	defaultTaskUIDMax      uint32 = 265535
	maximumTaskUIDPoolSize        = 1 << 20
)

// executionUIDPool assigns kernel identities to active assignments without
// creating persistent passwd entries. A UID is not reusable until the previous
// assignment workspace has been sealed back to root ownership.
type executionUIDPool struct {
	mu          sync.Mutex
	minimum     uint32
	maximum     uint32
	next        uint32
	assignments map[string]uint32
	active      map[uint32]string
	blocked     func(uint32) bool
}

func newExecutionUIDPool(minimum, maximum uint32, blocked func(uint32) bool) (*executionUIDPool, error) {
	if minimum == 0 || maximum < minimum || uint64(maximum)-uint64(minimum)+1 > maximumTaskUIDPoolSize {
		return nil, errors.New("temporary task UID range is invalid or too large")
	}
	if blocked == nil {
		blocked = hostUIDExists
	}
	return &executionUIDPool{
		minimum: minimum, maximum: maximum, next: minimum,
		assignments: make(map[string]uint32), active: make(map[uint32]string), blocked: blocked,
	}, nil
}

func (pool *executionUIDPool) Allocate(key string) (uint32, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if uid, ok := pool.assignments[key]; ok {
		return uid, nil
	}
	size := uint64(pool.maximum) - uint64(pool.minimum) + 1
	candidate := pool.next
	for checked := uint64(0); checked < size; checked++ {
		if _, active := pool.active[candidate]; !active && !pool.blocked(candidate) {
			pool.assignments[key] = candidate
			pool.active[candidate] = key
			if candidate == pool.maximum {
				pool.next = pool.minimum
			} else {
				pool.next = candidate + 1
			}
			return candidate, nil
		}
		if candidate == pool.maximum {
			candidate = pool.minimum
		} else {
			candidate++
		}
	}
	return 0, errors.New("temporary task UID pool is exhausted")
}

func (pool *executionUIDPool) Release(key string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	uid, ok := pool.assignments[key]
	if !ok {
		return
	}
	delete(pool.assignments, key)
	delete(pool.active, uid)
}

func hostUIDExists(uid uint32) bool {
	_, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err == nil {
		return true
	}
	var unknown user.UnknownUserIdError
	return !errors.As(err, &unknown)
}

func executionCredential(template *syscall.Credential, uid uint32) (*syscall.Credential, error) {
	if template == nil || uid == 0 || template.Gid == 0 {
		return nil, errors.New("temporary task credential is incomplete")
	}
	groups := append([]uint32(nil), template.Groups...)
	return &syscall.Credential{Uid: uid, Gid: template.Gid, Groups: groups}, nil
}

func temporaryUIDRange(minimum, maximum uint32) (uint32, uint32, error) {
	if minimum == 0 && maximum == 0 {
		return defaultTaskUIDMin, defaultTaskUIDMax, nil
	}
	if minimum == 0 || maximum == 0 {
		return 0, 0, fmt.Errorf("both temporary task UID range bounds are required")
	}
	if _, err := newExecutionUIDPool(minimum, maximum, func(uint32) bool { return false }); err != nil {
		return 0, 0, err
	}
	return minimum, maximum, nil
}
