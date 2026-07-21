package rpc

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
)

// JSON-RPC 2.0 error codes (from the spec).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// -32000..-32099 are reserved for implementation-defined server errors.
	CodeServerError = -32000
)

// Limits. The RPC port is trivially reachable; every bound here exists because
// its absence is a free denial-of-service.
const (
	maxBodyBytes   = 1 << 20 // 1 MiB
	maxBatchLength = 64
	// maxBatchResponseBytes caps the CUMULATIVE serialized size of a batch's
	// responses. The 1 MiB body + 64-call limits bound the REQUEST, but a batch of
	// 64 eth_getLogs (each allowed a 10k-block range) has no matching bound on what
	// comes back: a tiny request amplifies into hundreds of MB of response, an OOM
	// from one packet. Once the accumulated output crosses this budget the remaining
	// calls are answered with an error and NOT executed, so the expensive log scans
	// never run.
	maxBatchResponseBytes = 10 << 20 // 10 MiB
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// MarshalJSON enforces JSON-RPC 2.0: a SUCCESS response must always carry "result" — even
// when it is null. With `omitempty`, a null result (a pending eth_getTransactionReceipt, a
// block miss) serialized to {"jsonrpc":..,"id":..} with no "result" member, and strict
// clients that branch on the presence of result vs error — MetaMask/ethers "is my tx mined?"
// polling — see neither and can hang. An ERROR response carries "error" and no "result".
func (r response) MarshalJSON() ([]byte, error) {
	if r.Error != nil {
		return json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			Error   *rpcError       `json:"error"`
			ID      json.RawMessage `json:"id"`
		}{r.JSONRPC, r.Error, r.ID})
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  interface{}     `json:"result"`
		ID      json.RawMessage `json:"id"`
	}{r.JSONRPC, r.Result, r.ID})
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Err builds a server error that will be reported to the caller verbatim.
func Err(code int, msg string) error { return &rpcError{Code: code, Message: msg} }

// Handler is a single RPC method.
type Handler func(params json.RawMessage) (interface{}, error)

// Server is a minimal JSON-RPC 2.0 server over HTTP.
type Server struct {
	mu      sync.RWMutex
	methods map[string]Handler
}

func NewServer() *Server {
	return &Server{methods: make(map[string]Handler)}
}

func (s *Server) Register(name string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[name] = h
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// MaxBytesReader, not io.LimitReader: it errors past the limit instead of
	// silently truncating, which would turn an oversized request into a confusing
	// parse error.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, response{JSONRPC: "2.0", Error: &rpcError{CodeInvalidRequest, "request too large"}, ID: json.RawMessage("null")})
		return
	}

	// A batch is a JSON array; a single call is an object.
	trimmed := firstNonSpace(body)
	if trimmed == '[' {
		var reqs []request
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, response{JSONRPC: "2.0", Error: &rpcError{CodeParseError, "parse error"}, ID: json.RawMessage("null")})
			return
		}
		if len(reqs) > maxBatchLength {
			writeJSON(w, response{JSONRPC: "2.0", Error: &rpcError{CodeInvalidRequest, "batch too large"}, ID: json.RawMessage("null")})
			return
		}
		// Serialize each response as we go and accumulate its size. Marshalling here
		// (once) instead of letting writeJSON do it lets us both measure the running
		// total AND stop dispatching once the budget is blown — the response that
		// tips us over still completes (bounded by the per-call getLogs cap), but no
		// further handler runs. out holds pre-marshalled entries so writeJSON does
		// not re-encode the large payloads.
		out := make([]json.RawMessage, 0, len(reqs))
		var total int
		for _, req := range reqs {
			var res response
			if total >= maxBatchResponseBytes {
				id := req.ID
				if id == nil {
					id = json.RawMessage("null")
				}
				res = response{JSONRPC: "2.0", Error: &rpcError{CodeServerError, "batch response size limit exceeded"}, ID: id}
			} else {
				res = s.dispatch(req)
			}
			b, err := json.Marshal(res)
			if err != nil {
				b = json.RawMessage(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"},"id":null}`)
			}
			total += len(b)
			out = append(out, b)
		}
		writeJSON(w, out)
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, response{JSONRPC: "2.0", Error: &rpcError{CodeParseError, "parse error"}, ID: json.RawMessage("null")})
		return
	}
	writeJSON(w, s.dispatch(req))
}

func (s *Server) dispatch(req request) response {
	id := req.ID
	if id == nil {
		id = json.RawMessage("null")
	}
	res := response{JSONRPC: "2.0", ID: id}

	if req.JSONRPC != "2.0" {
		res.Error = &rpcError{CodeInvalidRequest, "jsonrpc must be \"2.0\""}
		return res
	}

	s.mu.RLock()
	h, ok := s.methods[req.Method]
	s.mu.RUnlock()
	if !ok {
		res.Error = &rpcError{CodeMethodNotFound, "method not found: " + req.Method}
		return res
	}

	result, err := s.invoke(h, req.Method, req.Params)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			res.Error = re
		} else {
			// Internal error text is a fingerprinting aid and should not leak
			// in a real deployment; kept verbose here for devnet debugging.
			res.Error = &rpcError{CodeServerError, err.Error()}
		}
		return res
	}
	res.Result = result
	return res
}

// invoke calls a handler behind a panic firewall. A malformed request that trips
// a nil-deref or out-of-range slice must not take the node down (or abort with a
// lock held): that is a one-packet denial of service. A recovered panic becomes
// a generic internal error (no stack trace leaked to the caller) and is logged
// for the operator.
func (s *Server) invoke(h Handler, method string, params json.RawMessage) (result interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("rpc: recovered from panic in method %q: %v", method, r)
			err = &rpcError{CodeInternalError, "internal error"}
		}
	}()
	return h(params)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonSpace(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}
