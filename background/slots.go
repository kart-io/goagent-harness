package background

import (
	"context"
	"sync"
)

// Priority 决定等待时的先后。
type Priority int

const (
	// PriorityHuman 是人的消息或状态请求，可以使用保留槽位。
	PriorityHuman Priority = iota
	// PriorityWake 是唤醒已有工作的自动输入（如 CI 结果）。
	PriorityWake
	// PriorityLaunch 是新任务。
	PriorityLaunch
)

// reservedSlots 是只留给人的输入的槽位数，使新任务占满时人仍能继续已有工作。
const reservedSlots = 1

type slotWaiter struct {
	priority Priority
	ready    chan struct{}
}

// Slots 是带优先级的并发槽位。占用中的轮次不会被抢占。
type Slots struct {
	capacity int
	mu       sync.Mutex
	used     int
	waiters  []*slotWaiter
}

// NewSlots 创建有 capacity 个普通槽位和一个保留槽位的 Slots。
func NewSlots(capacity int) *Slots { return &Slots{capacity: capacity} }

// Used 返回正在占用的槽位数。
func (s *Slots) Used() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

func (s *Slots) available(priority Priority) bool {
	if priority == PriorityHuman {
		return s.used < s.capacity+reservedSlots
	}
	return s.used < s.capacity
}

// Acquire 等待槽位，返回释放函数；重复释放被忽略。
func (s *Slots) Acquire(ctx context.Context, priority Priority) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.available(priority) && !s.hasEarlier(priority) {
		s.used++
		s.mu.Unlock()
		return s.releaser(), nil
	}
	waiter := &slotWaiter{priority: priority, ready: make(chan struct{})}
	s.waiters = append(s.waiters, waiter)
	s.mu.Unlock()
	select {
	case <-waiter.ready:
		return s.releaser(), nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, w := range s.waiters {
			if w == waiter {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				return nil, ctx.Err()
			}
		}
		// 已被分配：归还，交给下一个等待者。
		s.used--
		s.dispatch()
		return nil, ctx.Err()
	}
}

// hasEarlier 报告是否有不低于该优先级的等待者。
func (s *Slots) hasEarlier(priority Priority) bool {
	for _, w := range s.waiters {
		if w.priority <= priority {
			return true
		}
	}
	return false
}

func (s *Slots) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.used--
			s.dispatch()
		})
	}
}

// dispatch 把空出的槽位按优先级、同级按到达顺序分配。调用方持有 s.mu。
func (s *Slots) dispatch() {
	for {
		best := -1
		for i, w := range s.waiters {
			if s.available(w.priority) && (best < 0 || w.priority < s.waiters[best].priority) {
				best = i
			}
		}
		if best < 0 {
			return
		}
		waiter := s.waiters[best]
		s.waiters = append(s.waiters[:best], s.waiters[best+1:]...)
		s.used++
		close(waiter.ready)
	}
}
