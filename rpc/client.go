package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Client is a JSON-RPC 2.0 client.
type Client struct {
	url  string
	http *http.Client
	id   atomic.Uint64
}

func NewClient(url string) *Client {
	return &Client{
		url:  url,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Call invokes a method and unmarshals the result into out.
// Pass out == nil to ignore the result.
func (c *Client) Call(method string, out interface{}, params ...interface{}) error {
	if params == nil {
		params = []interface{}{}
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := c.id.Add(1)
	body, err := json.Marshal(request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
	})
	if err != nil {
		return err
	}

	resp, err := c.http.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rpc: %w (is the node running?)", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}

	var res struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("rpc: bad response: %s", string(raw))
	}
	if res.Error != nil {
		return fmt.Errorf("rpc: %s (code %d)", res.Error.Message, res.Error.Code)
	}
	if out == nil {
		return nil
	}
	if len(res.Result) == 0 || string(res.Result) == "null" {
		return ErrNullResult
	}
	return json.Unmarshal(res.Result, out)
}

// ErrNullResult means the node answered successfully with "no such thing" — not
// a failure: an unmined tx has no receipt, which is the correct answer.
var ErrNullResult = fmt.Errorf("rpc: null result")
