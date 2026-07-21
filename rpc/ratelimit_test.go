package rpc

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// okHandler is the thing the limiter protects: it just records that it was
// reached. If the limiter denies a request, this never runs.
func okHandler() (http.Handler, *int) {
	hits := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	return h, &hits
}

// fire sends one request from remoteAddr through the limiter and returns the
// HTTP status the client saw.
func fire(rl *RateLimiter, remoteAddr string) int {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	rl.ServeHTTP(rec, req)
	return rec.Code
}

// TestBurstThenThrottle: an IP may spend its whole burst back-to-back, then the
// next request is refused with 429; the protected handler is never reached for
// the refused one.
func TestBurstThenThrottle(t *testing.T) {
	clk := time.Unix(0, 0)
	next, hits := okHandler()
	rl := NewRateLimiter(next, RateLimit{Burst: 3, PerSecond: 1, MaxTracked: 16}, func() time.Time { return clk })

	for i := 0; i < 3; i++ {
		if code := fire(rl, "10.0.0.1:1111"); code != http.StatusOK {
			t.Fatalf("burst request %d: got %d, want 200", i, code)
		}
	}
	if code := fire(rl, "10.0.0.1:2222"); code != http.StatusTooManyRequests {
		t.Fatalf("post-burst request: got %d, want 429", code)
	}
	if *hits != 3 {
		t.Fatalf("protected handler reached %d times, want 3 (the 4th must be blocked before it)", *hits)
	}
}

// TestRefillOverTime: once throttled, advancing the clock credits tokens at the
// configured rate and the IP is allowed again, but not before enough time has
// passed.
func TestRefillOverTime(t *testing.T) {
	clk := time.Unix(0, 0)
	next, _ := okHandler()
	rl := NewRateLimiter(next, RateLimit{Burst: 2, PerSecond: 1, MaxTracked: 16}, func() time.Time { return clk })

	ip := "10.0.0.5:9000"
	fire(rl, ip)
	fire(rl, ip)
	if code := fire(rl, ip); code != http.StatusTooManyRequests {
		t.Fatalf("after draining burst: got %d, want 429", code)
	}

	// Half a second at 1 token/s is 0.5 tokens: below 1, still denied.
	clk = clk.Add(500 * time.Millisecond)
	if code := fire(rl, ip); code != http.StatusTooManyRequests {
		t.Fatalf("after 0.5s (0.5 tokens): got %d, want still 429", code)
	}

	// Another half-second crosses a whole token: allowed exactly once.
	clk = clk.Add(500 * time.Millisecond)
	if code := fire(rl, ip); code != http.StatusOK {
		t.Fatalf("after 1s total (1 token): got %d, want 200", code)
	}
	if code := fire(rl, ip); code != http.StatusTooManyRequests {
		t.Fatalf("immediately after spending the refilled token: got %d, want 429", code)
	}
}

// TestPerIPIsolation: one IP exhausting its bucket must not affect another. A
// shared key would let a single greedy client throttle everyone.
func TestPerIPIsolation(t *testing.T) {
	clk := time.Unix(0, 0)
	next, _ := okHandler()
	rl := NewRateLimiter(next, RateLimit{Burst: 1, PerSecond: 1, MaxTracked: 16}, func() time.Time { return clk })

	if code := fire(rl, "1.1.1.1:100"); code != http.StatusOK {
		t.Fatalf("A first request: got %d, want 200", code)
	}
	if code := fire(rl, "1.1.1.1:100"); code != http.StatusTooManyRequests {
		t.Fatalf("A second request: got %d, want 429 (A is exhausted)", code)
	}
	// B is a different IP and must be completely unaffected by A's spending.
	if code := fire(rl, "2.2.2.2:200"); code != http.StatusOK {
		t.Fatalf("B first request: got %d, want 200 (B has its own bucket)", code)
	}
}

// TestForwardedForHeaderIsNotTrusted pins the security property: the bucket key
// is the real socket peer, never a client-supplied header. Honouring
// X-Forwarded-For would let one host forge a fresh source per request and mint
// unlimited full buckets, defeating the limiter.
func TestForwardedForHeaderIsNotTrusted(t *testing.T) {
	clk := time.Unix(0, 0)
	next, _ := okHandler()
	rl := NewRateLimiter(next, RateLimit{Burst: 1, PerSecond: 1, MaxTracked: 16}, func() time.Time { return clk })

	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.RemoteAddr = "9.9.9.9:5000"
	req1.Header.Set("X-Forwarded-For", "1.2.3.4")
	rec1 := httptest.NewRecorder()
	rl.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec1.Code)
	}

	// Same socket peer, different forged forwarding header. Must be throttled,
	// proving the header did not mint a new bucket.
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "9.9.9.9:5001"
	req2.Header.Set("X-Forwarded-For", "5.6.7.8")
	rec2 := httptest.NewRecorder()
	rl.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request with a different X-Forwarded-For: got %d, want 429 (the header must not create a new bucket)", rec2.Code)
	}
}

// TestTrackingMapIsBounded: pushing many distinct IPs through must not grow the
// map without limit. The map itself is a memory-DoS surface; MaxTracked caps it
// and evict() enforces the cap.
func TestTrackingMapIsBounded(t *testing.T) {
	clk := time.Unix(0, 0)
	next, _ := okHandler()
	const cap = 4
	rl := NewRateLimiter(next, RateLimit{Burst: 1, PerSecond: 1, MaxTracked: cap}, func() time.Time { return clk })

	// 200 distinct IPs, each firing once (draining its single-token bucket so no
	// bucket is ever "full" and cheaply reclaimable), forcing the oldest-eviction
	// path.
	for i := 0; i < 200; i++ {
		fire(rl, ipForIndex(i))
	}

	rl.mu.Lock()
	size := len(rl.buckets)
	rl.mu.Unlock()
	if size > cap {
		t.Fatalf("tracking map holds %d buckets, must never exceed MaxTracked=%d (an unbounded map is the DoS)", size, cap)
	}
}

func ipForIndex(i int) string {
	return "10.0." + itoa(i/256) + "." + itoa(i%256) + ":1234"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	p := len(buf)
	for n > 0 {
		p--
		buf[p] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[p:])
}
