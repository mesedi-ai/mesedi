// Per-project token-bucket rate limiter.
//
// The rate limiter sits in the auth chain after authMiddleware (which
// attaches project_id to the request context) and schemaVersionMiddleware
// (which cheaply rejects malformed wire-format requests). It is the last
// gate before the request reaches the actual handler.
//
// Implementation: a token bucket per project, kept in an in-memory map
// protected by a sync.RWMutex. Each bucket is itself protected by its
// own sync.Mutex so that high-frequency requests against the same
// project don't serialize on the outer map lock.
//
// In-memory storage is sufficient for local development and for a
// single-instance production deployment. When Mesedi scales horizontally,
// the in-memory map gets swapped for a Redis-backed implementation
// (sharing the same interface). The interface boundary is the
// `take()` method on `tokenBucket`; that contract stays stable across
// implementations.
//
// Defaults today: 100 burst capacity, 10 tokens refilled per second.
// That is generous enough that a well-behaved SDK (batches of ~100 events
// flushed at most every few hundred ms) never sees a 429, while tight
// enough that an obvious bug — infinite-loop agent with no retry
// backoff — surfaces a 429 within a second or two.
//
// Per-project overrides will eventually come from the `projects` table
// (rate_limit_capacity, rate_limit_refill_per_sec columns). For now
// every project gets the same default.
package api

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultRateLimitCapacity is the burst capacity (tokens at a fresh
// bucket / max tokens after refill). A request consumes 1 token.
const DefaultRateLimitCapacity = 100.0

// DefaultRateLimitRefillPerSec is the sustained throughput: tokens
// added to a non-full bucket per real second. At 10/sec, a 100-token
// bucket fully refills in 10 seconds after being drained.
const DefaultRateLimitRefillPerSec = 10.0

// tokenBucket is a single-project token bucket. Safe for concurrent
// access via its internal mutex.
type tokenBucket struct {
	mu           sync.Mutex
	tokens       float64
	capacity     float64
	refillPerSec float64
	lastRefill   time.Time
}

// newBucket returns a token bucket that starts full. Starting full is
// the user-friendly choice: a project's first request never gets a 429
// just because the bucket was empty at boot.
func newBucket(capacity, refillPerSec float64) *tokenBucket {
	return &tokenBucket{
		tokens:       capacity,
		capacity:     capacity,
		refillPerSec: refillPerSec,
		lastRefill:   time.Now(),
	}
}

// take attempts to consume 1 token. The bucket is first refilled based
// on the elapsed time since lastRefill, capped at capacity. If at least
// 1 token is then available, it is consumed and ok=true is returned.
// Otherwise ok=false (the caller should respond 429).
//
// Returns the post-call token count (for X-RateLimit-Remaining) and
// the time until the bucket would refill to full from the current
// level (for the Retry-After / X-RateLimit-Reset hints).
func (b *tokenBucket) take() (ok bool, remaining float64, fullRefillIn time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSec)
	b.lastRefill = now

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		ok = true
	}
	remaining = b.tokens

	deficit := b.capacity - b.tokens
	fullRefillIn = time.Duration(deficit / b.refillPerSec * float64(time.Second))
	return ok, remaining, fullRefillIn
}

// rateLimiter holds the per-project bucket map. Read-mostly access
// pattern (the outer map is rarely mutated once a project's bucket
// exists), so a sync.RWMutex keeps the lookup hot path lock-free
// during the common case.
type rateLimiter struct {
	mu           sync.RWMutex
	buckets      map[string]*tokenBucket
	capacity     float64
	refillPerSec float64
}

func newRateLimiter(capacity, refillPerSec float64) *rateLimiter {
	return &rateLimiter{
		buckets:      make(map[string]*tokenBucket),
		capacity:     capacity,
		refillPerSec: refillPerSec,
	}
}

// bucketFor returns the bucket for projectID, creating it lazily on
// first use. Double-checked locking pattern: the common case (bucket
// already exists) takes only a read lock; bucket creation upgrades
// to a write lock and re-verifies under the write lock so two callers
// arriving simultaneously don't both create a bucket.
func (r *rateLimiter) bucketFor(projectID string) *tokenBucket {
	r.mu.RLock()
	b, ok := r.buckets[projectID]
	r.mu.RUnlock()
	if ok {
		return b
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.buckets[projectID]; ok {
		return b // another goroutine beat us to it
	}
	b = newBucket(r.capacity, r.refillPerSec)
	r.buckets[projectID] = b
	return b
}

// rateLimitMiddleware enforces a token-bucket per project. Must run
// AFTER authMiddleware (which attaches project_id to the request
// context); if no project_id is present, the request is passed through
// untouched as a defense-in-depth no-op (routing should never let
// this happen, but middleware should fail open rather than crash).
func rateLimitMiddleware(logger *slog.Logger) Middleware {
	limiter := newRateLimiter(DefaultRateLimitCapacity, DefaultRateLimitRefillPerSec)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			projectID, ok := ProjectIDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			bucket := limiter.bucketFor(projectID)
			allowed, remaining, resetIn := bucket.take()

			// Standard rate-limit headers, set before any WriteHeader call
			// so they're present on both 200 and 429 responses.
			w.Header().Set("X-RateLimit-Limit", strconv.FormatFloat(limiter.capacity, 'f', 0, 64))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatFloat(math.Floor(remaining), 'f', 0, 64))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10))

			if !allowed {
				// Retry-After in whole seconds, minimum 1. The bucket
				// refills 1 token in (1 / refillPerSec) seconds, so
				// that's the earliest a retry could succeed.
				retryAfter := int(math.Ceil(1.0 / limiter.refillPerSec))
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				logger.Warn("rate limit exceeded",
					"project_id", projectID,
					"method", r.Method,
					"path", r.URL.Path,
					"retry_after_sec", retryAfter,
				)
				writeError(w, http.StatusTooManyRequests,
					"rate limit exceeded for project "+projectID)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
