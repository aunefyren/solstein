package exits

import (
	"context"
	"errors"
	"strings"
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
	pool.waitLimit = 0 // fail at once; TestPoolWaitsForRoom covers waiting
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
	if first.activeCount() != 2 {
		t.Errorf("get handed over %d uses, want one each", first.activeCount())
	}
	first.release()
	second.release()
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
	oldest.release() // the caller is done with it
	clock.advance(time.Minute)
	busy, _ := pool.get(ctx, Server{Name: "b"})
	busy.use()     // an open connection
	busy.release() // ... and the use get handed over is given back

	clock.advance(time.Minute)
	third, err := pool.get(ctx, Server{Name: "c"})
	if err != nil {
		t.Fatalf("get at the limit: %v", err)
	}
	third.release()
	oldestClosed, _ := oldest.shut()
	busyClosed, _ := busy.shut()
	if !oldestClosed || busyClosed {
		t.Errorf("wrong tunnel evicted: a closed %v, b closed %v", oldestClosed, busyClosed)
	}

	// Now "b" (busy) and "c" (idle but newest): c goes next.
	if _, err := pool.get(ctx, Server{Name: "d"}); err != nil {
		t.Fatal(err)
	}
	thirdClosed, _ := third.shut()
	busyClosed, _ = busy.shut()
	if !thirdClosed || busyClosed {
		t.Error("busy tunnel closed to make room")
	}
}

// A tunnel the pool has handed over is in use before its borrower has dialled
// anything: it may be writing its handshake through it, and closing the device
// under that packet panics the process rather than failing (see tunnel.close).
func TestPoolCannotEvictHandedOverTunnel(t *testing.T) {
	pool, clock, _ := newFakePool(t, 1)
	ctx := context.Background()
	handed, err := pool.get(ctx, Server{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute) // idle by the clock, but held

	if _, err := pool.get(ctx, Server{Name: "b"}); !errors.Is(err, ErrTunnelLimit) {
		t.Errorf("err = %v, want ErrTunnelLimit", err)
	}
	if closed, _ := handed.shut(); closed {
		t.Fatal("a tunnel handed over was closed to make room")
	}
	pool.reap()
	if closed, _ := handed.shut(); closed {
		t.Error("a tunnel handed over was reaped while in use")
	}

	handed.release()
	if _, err := pool.get(ctx, Server{Name: "b"}); err != nil {
		t.Errorf("after the use was given back: %v", err)
	}
}

// Closing a tunnel that is still in use marks it shut at once, so nothing new
// starts on it, and closes the device only once the last user has left.
func TestCloseWaitsForTheLastUser(t *testing.T) {
	pool, _, _ := newFakePool(t, 0)
	held, err := pool.get(context.Background(), Server{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	pool.forget("a")
	closed, deviceClosed := held.shut()
	if !closed || deviceClosed {
		t.Errorf("closed = %v, device closed = %v; want the device to wait", closed, deviceClosed)
	}
	if err := held.use(); !errors.Is(err, errTunnelShut) {
		t.Errorf("new use of a shut tunnel: err = %v", err)
	}

	held.release()
	if _, deviceClosed := held.shut(); !deviceClosed {
		t.Error("device not closed once the last user left")
	}
}

func TestPoolLimitWithEverythingBusy(t *testing.T) {
	pool, _, _ := newFakePool(t, 1)
	ctx := context.Background()
	only, _ := pool.get(ctx, Server{Name: "a"})
	only.use()
	only.release() // one open connection left

	if _, err := pool.get(ctx, Server{Name: "b"}); !errors.Is(err, ErrTunnelLimit) {
		t.Errorf("err = %v, want ErrTunnelLimit", err)
	}
	if closed, _ := only.shut(); closed {
		t.Error("busy tunnel closed")
	}

	only.release()
	next, err := pool.get(ctx, Server{Name: "b"})
	if err != nil {
		t.Errorf("after release: %v", err)
	} else {
		next.release()
	}
}

// At the limit with every tunnel busy, get waits for one to free up instead
// of failing: the tunnels in use are serving downloads that end, and the
// caller may be one half of a region-diff pair.
func TestPoolWaitsForRoom(t *testing.T) {
	pool, _, _ := newFakePool(t, 1)
	pool.waitLimit = time.Minute // the clock is frozen, so only room ends the wait
	ctx := context.Background()
	busy, err := pool.get(ctx, Server{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		busy.release()
	}()

	next, err := pool.get(ctx, Server{Name: "b"})
	if err != nil {
		t.Fatalf("get waiting for room: %v", err)
	}
	next.release()
	if closed, _ := busy.shut(); !closed {
		t.Error("the freed tunnel wasn't the one evicted")
	}
}

func TestPoolWaitEndsWithTheContext(t *testing.T) {
	pool, _, _ := newFakePool(t, 1)
	pool.waitLimit = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	held, err := pool.get(ctx, Server{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := pool.get(ctx, Server{Name: "b"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPoolWaitRunsOut(t *testing.T) {
	pool, clock, _ := newFakePool(t, 1)
	pool.waitLimit = time.Minute
	ctx := context.Background()
	held, err := pool.get(ctx, Server{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	go func() {
		time.Sleep(50 * time.Millisecond)
		clock.advance(2 * time.Minute) // the wait's minute passes
	}()

	_, err = pool.get(ctx, Server{Name: "b"})
	if !errors.Is(err, ErrTunnelLimit) {
		t.Errorf("err = %v, want ErrTunnelLimit", err)
	}
	if !strings.Contains(err.Error(), "waited 1m0s") {
		t.Errorf("error doesn't say how long it waited: %v", err)
	}
}

func TestPoolReapsIdleTunnels(t *testing.T) {
	pool, clock, _ := newFakePool(t, 0)
	ctx := context.Background()
	idle, _ := pool.get(ctx, Server{Name: "idle"})
	idle.release()
	busy, _ := pool.get(ctx, Server{Name: "busy"})
	busy.use()
	busy.release()

	clock.advance(4 * time.Minute)
	pool.reap()
	if closed, _ := idle.shut(); closed {
		t.Error("closed before the idle timeout")
	}

	clock.advance(2 * time.Minute)
	pool.reap()
	if closed, _ := idle.shut(); !closed {
		t.Error("idle tunnel not closed after 5 minutes")
	}
	if closed, _ := busy.shut(); closed {
		t.Error("tunnel with an open connection closed; a long download would be cut")
	}

	// Idle time counts from when the last connection closed.
	busy.release()
	clock.advance(4 * time.Minute)
	pool.reap()
	if closed, _ := busy.shut(); closed {
		t.Error("closed 4 minutes after its last connection ended")
	}
	clock.advance(time.Minute)
	pool.reap()
	if closed, _ := busy.shut(); !closed || pool.openCount() != 0 {
		t.Error("not closed 5 minutes after its last connection ended")
	}
}

func TestPoolForgetAndRun(t *testing.T) {
	pool, _, opened := newFakePool(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	first, _ := pool.get(ctx, Server{Name: "a"})
	first.release()
	pool.forget("a")
	pool.forget("never-opened")
	if closed, _ := first.shut(); !closed || pool.openCount() != 0 {
		t.Error("forget didn't close the tunnel")
	}
	again, _ := pool.get(ctx, Server{Name: "a"})
	again.release()
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
