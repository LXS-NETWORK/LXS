package rpc

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimit configures the per-client token bucket in front of the RPC handler.
// A zero field means "use the default": these are wired in code, not parsed, so
// an unset field is shorthand, not a silent fallback.
type RateLimit struct {
	// Burst is the bucket capacity — the most requests one IP may fire
	// back-to-back before it must wait for a refill.
	Burst int
	// PerSecond is the sustained refill rate (tokens added per second). The
	// long-run request rate one IP is allowed once its burst is spent.
	PerSecond float64
	// MaxTracked bounds how many distinct client IPs get buckets. An unbounded
	// IP-keyed map is itself the DoS the limiter exists to prevent: a flood from
	// many source addresses grows it until the node runs out of memory. Past this
	// many IPs the limiter evicts (see evict).
	MaxTracked int
}

// DefaultRateLimit is generous enough that a legitimate wallet or explorer
// polling head state never trips it, low enough that one client cannot pin a
// core parsing requests as fast as it can send them.
var DefaultRateLimit = RateLimit{Burst: 100, PerSecond: 50, MaxTracked: 8192}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is per-client-IP admission control in front of the real handler.
//
// A token bucket, not a fixed window: a fixed window lets a client fire its full
// quota at the end of window N and again at the start of N+1, 2x the rate across
// the boundary. A bucket refills continuously, so there is no boundary.
//
// The limit is per HTTP request, and a JSON-RPC batch is one request; batch size
// (maxBatchLength) and body size (maxBodyBytes) are capped separately, so a batch
// cannot smuggle unbounded work past this gate.
type RateLimiter struct {
	next       http.Handler
	burst      float64
	perSecond  float64
	maxTracked int
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewRateLimiter wraps next with per-IP admission control. A nil now defaults to
// time.Now; tests inject a clock so refill is deterministic without sleeping.
func NewRateLimiter(next http.Handler, cfg RateLimit, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	if cfg.Burst < 1 {
		cfg.Burst = DefaultRateLimit.Burst
	}
	if cfg.PerSecond <= 0 {
		cfg.PerSecond = DefaultRateLimit.PerSecond
	}
	if cfg.MaxTracked < 1 {
		cfg.MaxTracked = DefaultRateLimit.MaxTracked
	}
	return &RateLimiter{
		next:       next,
		burst:      float64(cfg.Burst),
		perSecond:  cfg.PerSecond,
		maxTracked: cfg.MaxTracked,
		now:        now,
		buckets:    make(map[string]*bucket),
	}
}

func (rl *RateLimiter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !rl.allow(clientIP(r), rl.now()) {
		writeTooManyRequests(w)
		return
	}
	rl.next.ServeHTTP(w, r)
}

// allow reports whether this IP may make one request now, debiting a token if
// so. The whole limiter; ServeHTTP is plumbing around it.
func (rl *RateLimiter) allow(ip string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		// A new IP starts with a full bucket. Make room first if at the tracking
		// cap, else the map is the DoS.
		if len(rl.buckets) >= rl.maxTracked {
			rl.evict(now)
		}
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[ip] = b
	} else {
		// Lazy refill: no per-IP ticker (its own resource sink). Credit the
		// time elapsed since this IP was last seen, capped at capacity.
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = math.Min(rl.burst, b.tokens+elapsed*rl.perSecond)
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// evict frees a slot in a full tracking map. Caller holds the lock. Two-stage,
// cheapest first:
//
//  1. Drop every bucket refilled back to capacity — equivalent to one never
//     created. The normal path; reclaims idle IPs for free.
//  2. If that freed nothing (every IP actively spending — a many-distinct-IP
//     flood), evict the least-recently-seen one. Graceful degradation: bounded
//     memory instead of OOM, at the cost of an evicted attacker occasionally
//     earning a fresh bucket.
func (rl *RateLimiter) evict(now time.Time) {
	var oldestIP string
	var oldest time.Time
	for ip, b := range rl.buckets {
		eff := math.Min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.perSecond)
		if eff >= rl.burst {
			delete(rl.buckets, ip)
			continue
		}
		if oldestIP == "" || b.last.Before(oldest) {
			oldestIP, oldest = ip, b.last
		}
	}
	if len(rl.buckets) >= rl.maxTracked && oldestIP != "" {
		delete(rl.buckets, oldestIP)
	}
}

// clientIP is the bucket key: normally the real socket peer, host only.
//
// X-Forwarded-For from an arbitrary client is NOT honoured — it is attacker-
// controlled, so trusting it blindly would let one host forge a distinct source
// per request and mint a fresh bucket each time, defeating the limiter.
//
// The one exception: when the direct peer is LOOPBACK, the request reached a
// loopback-bound RPC through a same-host reverse proxy (our Caddy) — the only
// thing that can. We then key on the LAST X-Forwarded-For entry, which is the
// client IP the proxy itself observed and appended AFTER any value the client
// tried to forge. Without this, every public request shares one bucket (the
// proxy's), so a single abuser exhausts the shared quota for everyone. Safe only
// because an external host cannot reach a loopback socket directly; on a directly
// exposed node the peer is never loopback and XFF stays ignored.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return host
}

func writeTooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Retry-After is advisory; clients and proxies honour it to back off.
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(response{
		JSONRPC: "2.0",
		Error:   &rpcError{CodeServerError, "rate limit exceeded"},
		ID:      json.RawMessage("null"),
	})
}
