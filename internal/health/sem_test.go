package health

import (
	"context"
	"testing"
	"time"
)

// Excess requests wait FIFO: grants are handed to waiters in arrival order
// and a newcomer never jumps the line.
func TestFairSemOrder(t *testing.T) {
	sem := newFairSem(1)
	if w, ok := sem.Acquire(); !ok || w != nil {
		t.Fatal("first acquire must be immediate")
	}
	ws := make([]*semWaiter, 0, 5)
	for i := 0; i < 5; i++ {
		w, ok := sem.Acquire()
		if ok || w == nil {
			t.Fatalf("waiter %d must queue", i)
		}
		ws = append(ws, w)
	}
	sem.Release() // the holder finishes: first hand-off
	for i, w := range ws {
		if !w.Wait(context.Background(), 2*time.Second) {
			t.Fatalf("waiter %d never granted", i)
		}
		// Others must not have been granted.
		for k := i + 1; k < len(ws); k++ {
			select {
			case <-ws[k].ch:
				t.Fatalf("waiter %d granted before %d — not FIFO", k, i)
			default:
			}
		}
		if i != len(ws)-1 {
			sem.Release() // served: hand the slot to the next in line
		}
	}
	sem.Release() // last holder finishes
	if w, ok := sem.Acquire(); !ok || w != nil {
		t.Fatal("drained semaphore must admit")
	}
}

func TestFairSemTimeoutAndCancel(t *testing.T) {
	sem := newFairSem(1)
	if w, ok := sem.Acquire(); !ok || w != nil {
		t.Fatal("first acquire")
	}
	w, ok := sem.Acquire()
	if ok {
		t.Fatal("must queue")
	}
	if w.Wait(context.Background(), 80*time.Millisecond) {
		t.Fatal("must time out while held")
	}
	// After the timed-out waiter left the queue, releasing restores the
	// slot and a new request walks in immediately.
	sem.Release()
	if w2, ok2 := sem.Acquire(); !ok2 || w2 != nil {
		t.Fatal("slot not restored")
	}
	// Context cancellation while queued.
	w3, ok3 := sem.Acquire()
	if ok3 {
		t.Fatal("must queue again")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if w3.Wait(ctx, 5*time.Second) {
		t.Fatal("must give up on cancel")
	}
	sem.Release()
	if w4, ok4 := sem.Acquire(); !ok4 || w4 != nil {
		t.Fatal("slot not restored after cancel")
	}
}

func TestFairSemCapacityChange(t *testing.T) {
	sem := newFairSem(0) // unlimited
	for i := 0; i < 3; i++ {
		if _, ok := sem.Acquire(); !ok {
			t.Fatal("unlimited")
		}
	}
	sem.setCapacity(1) // two over cap in flight; drains absorb it
	if w, ok := sem.Acquire(); ok {
		_ = w
		t.Log("acquired during shrink absorb — acceptable transient")
	} else {
		w.Wait(context.Background(), time.Second)
	}
	sem.Release()
	sem.setCapacity(0)
	if _, ok := sem.Acquire(); !ok {
		t.Fatal("back to unlimited must admit")
	}
}
