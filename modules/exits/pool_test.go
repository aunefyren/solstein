package exits

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for idle timing.
type fakeClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *fakeClock) advance(duration time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(duration)
}

// newFakePool is a pool whose tunnels are bookkeeping only, no WireGuard.
func newFakePool(t *testing.T, max int) (*pool, *fakeClock, *int) {
	t.Helper()
	quietLogs(t)
	clock := &fakeClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	pool := newPool("test", max, nil, nil, clock.Now)
	opened := 0
	pool.open = func(ctx context.Context, server Server) (*tunnel, error) {
		if server.Name == "unreachable" {
			return nil, errors.New("no route")
		}
		opened++
		return &tunnel{server: server, now: clock.Now, lastUsed: clock.Now(), keyIndex: -1}, nil
	}
	return pool, clock, &opened
}

func TestPoolReusesTunnels(t *testing.T) {
	pool, _, opened := newFakePool(t, 0)
	ctx := context.Background()
	first, err := pool.get(ctx, Server{Name: "se-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := pool.get(ctx, Server{Name: "se-1"})
	if first != second || *opened != 1 {
		t.Errorf("tunnel not reused: opened %d", *opened)
	}
	if _, err := pool.get(ctx, Server{Name: "unreachable"}); err == nil {
		t.Error("failed open not reported")
	}
	if pool.openCount() != 1 {
		t.Errorf("open = %d, want 1", pool.openCount())
	}
}

func TestPoolLimitEvictsIdleTunnel(t *testing.T) {
	pool, clock, _ := newFakePool(t, 2)
	ctx := context.Background()
	oldest, _ := pool.get(ctx, Server{Name: "a"})
	clock.advance(time.Minute)
	busy, _ := pool.get(ctx, Server{Name: "b"})
	busy.use() // an open connection

	clock.advance(time.Minute)
	if _, err := pool.get(ctx, Server{Name: "c"}); err != nil {
		t.Fatalf("get at the limit: %v", err)
	}
	if !oldest.closed || busy.closed {
		t.Errorf("wrong tunnel evicted: a closed %v, b closed %v", oldest.closed, busy.closed)
	}

	// Now "b" (busy) and "c" (idle but newest): c goes next.
	third := pool.tunnels["c"]
	if _, err := pool.get(ctx, Server{Name: "d"}); err != nil {
		t.Fatal(err)
	}
	if !third.closed || busy.closed {
		t.Error("busy tunnel closed to make room")
	}
}

func TestPoolLimitWithEverythingBusy(t *testing.T) {
	pool, _, _ := newFakePool(t, 1)
	ctx := context.Background()
	only, _ := pool.get(ctx, Server{Name: "a"})
	only.use()

	if _, err := pool.get(ctx, Server{Name: "b"}); !errors.Is(err, ErrTunnelLimit) {
		t.Errorf("err = %v, want ErrTunnelLimit", err)
	}
	if only.closed {
		t.Error("busy tunnel closed")
	}

	only.release()
	if _, err := pool.get(ctx, Server{Name: "b"}); err != nil {
		t.Errorf("after release: %v", err)
	}
}

func TestPoolReapsIdleTunnels(t *testing.T) {
	pool, clock, _ := newFakePool(t, 0)
	ctx := context.Background()
	idle, _ := pool.get(ctx, Server{Name: "idle"})
	busy, _ := pool.get(ctx, Server{Name: "busy"})
	busy.use()

	clock.advance(4 * time.Minute)
	pool.reap()
	if idle.closed {
		t.Error("closed before the idle timeout")
	}

	clock.advance(2 * time.Minute)
	pool.reap()
	if !idle.closed {
		t.Error("idle tunnel not closed after 5 minutes")
	}
	if busy.closed {
		t.Error("tunnel with an open connection closed; a long download would be cut")
	}

	// Idle time counts from when the last connection closed.
	busy.release()
	clock.advance(4 * time.Minute)
	pool.reap()
	if busy.closed {
		t.Error("closed 4 minutes after its last connection ended")
	}
	clock.advance(time.Minute)
	pool.reap()
	if !busy.closed || pool.openCount() != 0 {
		t.Error("not closed 5 minutes after its last connection ended")
	}
}

func TestPoolForgetAndRun(t *testing.T) {
	pool, _, opened := newFakePool(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	first, _ := pool.get(ctx, Server{Name: "a"})
	pool.forget("a")
	pool.forget("never-opened")
	if !first.closed || pool.openCount() != 0 {
		t.Error("forget didn't close the tunnel")
	}
	pool.get(ctx, Server{Name: "a"})
	if *opened != 2 {
		t.Errorf("forgotten tunnel not reopened: opened %d", *opened)
	}

	done := make(chan struct{})
	go func() {
		pool.run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if pool.openCount() != 0 {
		t.Error("tunnels left open after shutdown")
	}
}
