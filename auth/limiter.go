package auth

import (
	"sync"
	"time"
)

// Failed sign-in attempts allowed within limitWindow before further ones are
// refused, per username and per client address. A wrong TOTP code counts as
// well as a wrong password.
const (
	limitWindow  = 15 * time.Minute
	limitPerUser = 5
	limitPerIP   = 20
)

// limiter counts failed attempts per key, in memory: a restart forgets them,
// which is acceptable against online guessing, the threat it's for.
type limiter struct {
	mutex    sync.Mutex
	failures map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{failures: map[string][]time.Time{}}
}

// blocked reports whether a key has reached its limit, and until when.
func (limiter *limiter) blocked(key string, limit int, now time.Time) (bool, time.Time) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	recent := limiter.prune(key, now)
	if len(recent) < limit {
		return false, time.Time{}
	}
	return true, recent[0].Add(limitWindow)
}

func (limiter *limiter) fail(key string, now time.Time) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	limiter.failures[key] = append(limiter.prune(key, now), now)
}

func (limiter *limiter) reset(key string) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	delete(limiter.failures, key)
}

// prune drops a key's failures older than the window; the caller holds the
// lock.
func (limiter *limiter) prune(key string, now time.Time) []time.Time {
	recent := limiter.failures[key][:0:0]
	for _, at := range limiter.failures[key] {
		if now.Sub(at) < limitWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) == 0 {
		delete(limiter.failures, key)
		return nil
	}
	limiter.failures[key] = recent
	return recent
}
