package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestManager returns a Manager whose slot counter is fake, so the
// reservation logic can be tested without Docker. active simulates the number
// of containers Docker would report.
func newTestManager(max int, active *atomic.Int64) *Manager {
	return &Manager{
		maxSessions: max,
		maxTTL:      time.Hour,
		activeFn:    func(context.Context) (int, error) { return int(active.Load()), nil },
	}
}

// A burst of simultaneous creates must not exceed the limit: counting and
// claiming happen together.
func TestReserveBurstNeverExceedsLimit(t *testing.T) {
	var active atomic.Int64
	m := newTestManager(3, &active)

	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.reserve(context.Background(), 0)
			var le *LimitError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &le) && errors.Is(err, ErrLimit) && le.Max == 3:
				refused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 3 || refused.Load() != 37 {
		t.Fatalf("granted %d, refused %d; want 3 and 37", ok.Load(), refused.Load())
	}
}

// Once the container exists Docker counts it, so releasing the reservation
// must not leave the slot counted twice (or free it twice).
func TestReserveReleaseAccounting(t *testing.T) {
	var active atomic.Int64
	m := newTestManager(2, &active)

	rel1, err := m.reserve(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	active.Add(1) // container created: Docker now counts it
	rel1()
	rel1() // releasing twice must not free a second slot

	rel2, err := m.reserve(context.Background(), 0)
	if err != nil {
		t.Fatalf("second slot should be free: %v", err)
	}
	defer rel2()
	if _, err := m.reserve(context.Background(), 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("third reserve = %v, want a limit error (1 container + 1 in flight = 2 of 2)", err)
	}

	info, err := m.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.MaxSessions != 2 || info.Running != 2 || info.Available != 0 {
		t.Errorf("info = %+v", info)
	}
}

func TestReserveQueuesUntilASlotFrees(t *testing.T) {
	var active atomic.Int64
	m := newTestManager(1, &active)
	active.Store(1) // the only slot is taken

	got := make(chan error, 1)
	start := time.Now()
	go func() {
		rel, err := m.reserve(context.Background(), 5*time.Second)
		if err == nil {
			rel()
		}
		got <- err
	}()

	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("returned before a slot was free: %v", err)
	default:
	}
	active.Store(0) // the session ends
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("queued reserve failed: %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("took %s after the slot freed", d)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("queued reserve never got the freed slot")
	}
}

func TestReserveWaitTimesOutCleanly(t *testing.T) {
	var active atomic.Int64
	m := newTestManager(1, &active)
	active.Store(1)

	start := time.Now()
	_, err := m.reserve(context.Background(), 300*time.Millisecond)
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LimitError", err)
	}
	if le.Max != 1 || le.Active != 1 {
		t.Errorf("limit error = %+v", le)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Errorf("waited %s, want about 300ms", d)
	}
}

func TestReserveHonoursContextCancel(t *testing.T) {
	var active atomic.Int64
	m := newTestManager(1, &active)
	active.Store(1)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := m.reserve(ctx, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's error", err)
	}
}
