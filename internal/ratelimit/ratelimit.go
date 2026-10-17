// Package ratelimit is a per-client token-bucket rate limiter.
//
// Ported from office_convert/rate_limit.py. In-memory, keyed by client IP,
// LRU-bounded at maxKeys. State is per-process (multi-replica gets N× the rate).
package ratelimit

import (
	"container/list"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Decision is the result of a rate-limit check.
type Decision struct {
	Allowed           bool
	Limit             int // tokens-per-minute ceiling
	Remaining         int // whole tokens left post-decision
	ResetEpochSeconds int64
	RetryAfterSeconds int // >=1 when denied
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a token bucket per client identifier. Refill rate = perMinute/60
// tokens/sec; capacity = burst; each request costs 1 token.
type Limiter struct {
	perMinute    int
	burst        int
	maxKeys      int
	refillPerSec float64

	mu      sync.Mutex
	buckets map[string]*list.Element // value: *lruEntry
	lru     *list.List
}

type lruEntry struct {
	key string
	b   bucket
}

// New constructs a Limiter. Panics on invalid params (matches Python's
// ValueError at construction).
func New(perMinute, burst, maxKeys int) *Limiter {
	if perMinute < 1 || burst < 1 || maxKeys < 1 {
		panic("ratelimit: per_minute/burst/max_keys must be >= 1")
	}
	return &Limiter{
		perMinute:    perMinute,
		burst:        burst,
		maxKeys:      maxKeys,
		refillPerSec: float64(perMinute) / 60.0,
		buckets:      make(map[string]*list.Element),
		lru:          list.New(),
	}
}

// Check atomically refills the client's bucket and tries to consume 1 token.
func (l *Limiter) Check(clientID string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	var b bucket
	if el, ok := l.buckets[clientID]; ok {
		ent := el.Value.(*lruEntry)
		b = ent.b
		b.tokens = minF(float64(l.burst), b.tokens+now.Sub(b.last).Seconds()*l.refillPerSec)
		l.lru.MoveToBack(el)
	} else {
		b = bucket{tokens: float64(l.burst)}
		if len(l.buckets) >= l.maxKeys {
			if oldest := l.lru.Front(); oldest != nil {
				old := oldest.Value.(*lruEntry)
				delete(l.buckets, old.key)
				l.lru.Remove(oldest)
			}
		}
	}

	allowed := b.tokens >= 1.0
	if allowed {
		b.tokens -= 1.0
	}
	b.last = now

	if el, ok := l.buckets[clientID]; ok {
		el.Value.(*lruEntry).b = b
	} else {
		l.buckets[clientID] = l.lru.PushBack(&lruEntry{key: clientID, b: b})
	}

	secondsToFull := (float64(l.burst) - b.tokens) / l.refillPerSec
	resetEpoch := time.Now().Add(time.Duration(secondsToFull * float64(time.Second))).Unix()

	retryAfter := 0
	if !allowed {
		secondsToOne := (1.0 - b.tokens) / l.refillPerSec
		retryAfter = int(secondsToOne) + 1
		if retryAfter < 1 {
			retryAfter = 1
		}
	}
	return Decision{
		Allowed:           allowed,
		Limit:             l.perMinute,
		Remaining:         int(b.tokens),
		ResetEpochSeconds: resetEpoch,
		RetryAfterSeconds: retryAfter,
	}
}

// ClientIDFor resolves a client identifier. With trustXFF, the LAST entry of
// X-Forwarded-For wins: the ALB appends the peer it saw to whatever the client
// sent, so earlier entries are client-controlled and keying on them let anyone
// mint a fresh bucket per request. Otherwise falls back to the remote address.
func ClientIDFor(r *http.Request, trustXFF bool) string {
	if trustXFF {
		xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
		if last := strings.TrimSpace(xff[strings.LastIndexByte(xff, ',')+1:]); last != "" {
			return last
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i] // strip port
	}
	if host == "" {
		return "unknown"
	}
	return host
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
