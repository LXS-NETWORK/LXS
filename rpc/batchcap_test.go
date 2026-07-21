package rpc

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A batch's REQUEST is bounded (1 MiB body, 64 calls) but its RESPONSE was not: 64
// eth_getLogs each returning a 10k-block span amplify a tiny request into an OOM.
// The cumulative-byte cap must stop DISPATCHING once the budget is blown, so the
// expensive handlers for the tail never run — not merely trim the output after the
// fact. This registers a handler that returns ~512 KiB and counts its invocations:
// a full 64-call batch would emit ~32 MiB, well over the 10 MiB budget, so only the
// prefix that fits may execute and the rest must come back as errors.
func TestBatchResponseSizeCapStopsDispatch(t *testing.T) {
	s := NewServer()
	var calls int32
	blob := strings.Repeat("x", 512*1024) // ~512 KiB per response
	s.Register("bloat", func(json.RawMessage) (interface{}, error) {
		atomic.AddInt32(&calls, 1)
		return blob, nil
	})

	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < maxBatchLength; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"jsonrpc":"2.0","method":"bloat","id":1}`)
	}
	sb.WriteByte(']')

	ts := httptest.NewServer(s)
	defer ts.Close()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(sb.String())))

	body := rec.Body.Bytes()
	// The whole point: the emitted body must stay near the budget, not balloon to
	// the full 32 MiB a 64×512 KiB batch would produce.
	if len(body) > maxBatchResponseBytes+2<<20 { // budget + one over-budget response of slack
		t.Fatalf("batch response = %d bytes, must be bounded near the %d budget", len(body), maxBatchResponseBytes)
	}

	var out []map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != maxBatchLength {
		t.Fatalf("got %d responses, want one per request (%d)", len(out), maxBatchLength)
	}

	// The expensive handler must have stopped being called well before all 64.
	ran := atomic.LoadInt32(&calls)
	if int(ran) >= maxBatchLength {
		t.Fatalf("handler ran %d times — the cap did not stop dispatch (unbounded response DoS)", ran)
	}

	// The tail must carry the size-limit error, not a silent truncation.
	sawLimit := false
	for _, r := range out {
		if e, ok := r["error"].(map[string]interface{}); ok {
			if msg, _ := e["message"].(string); strings.Contains(msg, "size limit") {
				sawLimit = true
			}
		}
	}
	if !sawLimit {
		t.Fatal("no response reported the size limit — the tail was silently dropped or all ran")
	}
}
