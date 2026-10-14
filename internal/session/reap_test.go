package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLimitedDoesSlowItemsTogetherButNotMoreThanTheLimit(t *testing.T) {
	var running, peak, done atomic.Int32
	var mu sync.Mutex
	items := make([]int, 16)
	start := time.Now()
	runLimited(8, items, func(int) {
		n := running.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // a slow stop
		running.Add(-1)
		done.Add(1)
	})
	if done.Load() != 16 {
		t.Fatalf("%d of 16 items were handled", done.Load())
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Errorf("peak concurrency %d, want between 2 and 8", p)
	}
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Errorf("took %v; 16 items of 100ms at 8 at a time should take about 200ms, not run in sequence", took)
	}
}
