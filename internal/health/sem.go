package health

import (
	"context"
	"sync"
	"time"
)

// fairSem is a FIFO semaphore: excess requests queue and wait for a free
// slot; capacity 0 means unlimited. Grants go directly to the longest
// waiting requester, so a queued request is never starved by a newcomer.
type fairSem struct {
	mu       sync.Mutex
	capacity int
	avail    int
	waiters  []*semWaiter
}

type semWaiter struct {
	sem       *fairSem
	ch        chan struct{} // buffered, size 1 — the grant
	cancelled bool
}

func newFairSem(capacity int) *fairSem {
	s := &fairSem{capacity: capacity}
	if capacity > 0 {
		s.avail = capacity
	}
	return s
}

func (s *fairSem) grantLocked(w *semWaiter) {
	w.ch <- struct{}{}
}

func (s *fairSem) popWaiterLocked() *semWaiter {
	for len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		if !w.cancelled {
			return w
		}
	}
	return nil
}

// drainLocked hands out any free slots to queued waiters first.
func (s *fairSem) drainLocked() {
	for s.avail > 0 {
		w := s.popWaiterLocked()
		if w == nil {
			return
		}
		s.avail--
		s.grantLocked(w)
	}
}

// setCapacity adjusts the cap; shrinking absorbs over-allocation in flight.
func (s *fairSem) setCapacity(c int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == s.capacity {
		return
	}
	old := s.capacity
	s.capacity = c
	if c == 0 {
		// now unlimited: everyone queued walks in
		for _, w := range s.waiters {
			if !w.cancelled {
				s.grantLocked(w)
			}
		}
		s.waiters = nil
		s.avail = 0
		return
	}
	if old == 0 {
		s.avail = c
	} else {
		s.avail += c - old
		if s.avail < 0 {
			s.avail = 0
		}
		if s.avail > c {
			s.avail = c
		}
	}
	s.drainLocked()
}

// Acquire takes a slot when free, else joins the FIFO queue. The returned
// waiter is nil when the slot was granted immediately.
func (s *fairSem) Acquire() (*semWaiter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capacity == 0 {
		return nil, true
	}
	s.drainLocked()
	if s.avail > 0 {
		s.avail--
		return nil, true
	}
	w := &semWaiter{sem: s, ch: make(chan struct{}, 1)}
	s.waiters = append(s.waiters, w)
	return w, false
}

// Wait parks until granted, timeout or ctx cancellation. A cancellation that
// races a grant yields acquired=true (the slot is held and must be
// released).
func (w *semWaiter) Wait(ctx context.Context, timeout time.Duration) (acquired bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.ch:
		return true
	case <-ctx.Done():
		return w.abandon()
	case <-timer.C:
		return w.abandon()
	}
}

func (w *semWaiter) abandon() bool {
	s := w.sem
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-w.ch:
		return true // grant already delivered: keep the slot
	default:
	}
	w.cancelled = true
	for i, x := range s.waiters {
		if x == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			break
		}
	}
	return false
}

// Release hands the slot to the longest waiter or restores it.
func (s *fairSem) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capacity == 0 {
		return
	}
	if w := s.popWaiterLocked(); w != nil {
		s.grantLocked(w) // direct hand-off; the slot never returns to the pool
		return
	}
	s.avail++
	if s.avail > s.capacity {
		s.avail = s.capacity
	}
}
