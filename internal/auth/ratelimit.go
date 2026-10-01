package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a keyed token-bucket rate limiter, e.g. one bucket per IP.
type Limiter struct {
	mu      sync.Mutex
	every   rate.Limit
	burst   int
	buckets map[string]*bucket
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLimiter allows burst events immediately, then one per interval, per key.
func NewLimiter(interval time.Duration, burst int) *Limiter {
	l := &Limiter{every: rate.Every(interval), burst: burst, buckets: map[string]*bucket{}}
	go l.cleanup()
	return l
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.every, l.burst)}
		l.buckets[key] = b
	}
	b.seen = time.Now()
	return b.lim.Allow()
}

func (l *Limiter) cleanup() {
	for range time.Tick(10 * time.Minute) {
		l.mu.Lock()
		for k, b := range l.buckets {
			if time.Since(b.seen) > time.Hour {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}
