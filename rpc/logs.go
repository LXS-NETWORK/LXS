package rpc

import (
	"encoding/json"
	"strconv"
	"strings"

	"lxs/common"
)

// maxLogRange bounds how many blocks a single eth_getLogs may scan. Without it a
// `fromBlock: 0, toBlock: latest` request walks the whole chain reading every
// receipt — unbounded I/O that pins a node. A blockHash query is exempt (one
// block).
const maxLogRange = 10_000

// ethLogFilter is the eth_getLogs filter object. address may be a single value
// or an array; each topic position may be null (any), a hash, or an array
// (any-of). blockHash, if set, selects one block and excludes fromBlock/toBlock.
type ethLogFilter struct {
	FromBlock string            `json:"fromBlock"`
	ToBlock   string            `json:"toBlock"`
	BlockHash *common.Hash      `json:"blockHash"`
	Address   json.RawMessage   `json:"address"`
	Topics    []json.RawMessage `json:"topics"`
}

// EthGetLogs returns every log in a block range matching an address set and a
// positional topic filter. Without it on-chain events are invisible off-chain,
// so explorers, DEX front-ends, and deposit-watchers cannot function.
func (a *API) EthGetLogs(params json.RawMessage) (interface{}, error) {
	var f ethLogFilter
	if err := decode(params, &f); err != nil {
		return nil, err
	}

	addrs, err := parseAddressFilter(f.Address)
	if err != nil {
		return nil, err
	}
	topics, err := parseTopicFilter(f.Topics)
	if err != nil {
		return nil, err
	}

	var from, to uint64
	if f.BlockHash != nil {
		// blockHash selects exactly one block; fromBlock/toBlock must not also be set.
		if f.FromBlock != "" || f.ToBlock != "" {
			return nil, Err(CodeInvalidParams, "blockHash cannot be combined with fromBlock/toBlock")
		}
		blk, err := a.bc.BlockByHash(*f.BlockHash)
		if err != nil {
			return nil, Err(CodeInvalidParams, "unknown blockHash")
		}
		from, to = blk.Header.Height, blk.Header.Height
	} else {
		head := a.bc.Head().Height()
		if from, err = a.resolveLogHeight(f.FromBlock, head, 0); err != nil {
			return nil, err
		}
		if to, err = a.resolveLogHeight(f.ToBlock, head, head); err != nil {
			return nil, err
		}
		if to < from {
			return nil, Err(CodeInvalidParams, "toBlock is before fromBlock")
		}
		if to-from >= maxLogRange {
			return nil, Err(CodeInvalidParams, "block range too wide (max "+strconv.Itoa(maxLogRange)+"); narrow fromBlock/toBlock")
		}
	}

	out := make([]interface{}, 0)
	for h := from; h <= to; h++ {
		receipts, blk, err := a.bc.ReceiptsByHeight(h)
		if err != nil {
			continue // a gap (shouldn't happen on the canonical range) is skipped, not fatal
		}
		logIndex := uint64(0) // per-block, across all txs — matches Ethereum
		for i, r := range receipts {
			var txHash common.Hash
			if i < len(blk.Txs) {
				txHash = blk.Txs[i].Hash()
			}
			for _, l := range r.Logs {
				if logMatches(l, addrs, topics) {
					out = append(out, map[string]interface{}{
						"address":          l.Address,
						"topics":           l.Topics,
						"data":             Data(l.Data),
						"blockNumber":      QU(blk.Header.Height),
						"blockHash":        blk.Hash(),
						"transactionHash":  txHash,
						"transactionIndex": QU(uint64(i)),
						"logIndex":         QU(logIndex),
						"removed":          false,
					})
				}
				logIndex++
			}
		}
	}
	return out, nil
}

// resolveLogHeight turns a block tag into a height. Empty defaults to `def`
// (fromBlock defaults to earliest=0, toBlock to latest) — the Ethereum defaults.
func (a *API) resolveLogHeight(tag string, head, def uint64) (uint64, error) {
	switch tag {
	case "":
		return def, nil
	case "latest", "pending", "safe", "finalized":
		return head, nil
	case "earliest":
		return 0, nil
	default:
		h, err := strconv.ParseUint(strings.TrimPrefix(tag, "0x"), 16, 64)
		if err != nil {
			return 0, Err(CodeInvalidParams, "bad block tag: "+tag)
		}
		return h, nil
	}
}

// parseAddressFilter accepts a single address, an array of addresses, or
// null/absent (match any). Returns nil for "match any".
func parseAddressFilter(raw json.RawMessage) (map[common.Address]bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	set := map[common.Address]bool{}
	// Try an array first, then a single address.
	var many []common.Address
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, a := range many {
			set[a] = true
		}
		return set, nil
	}
	var one common.Address
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, Err(CodeInvalidParams, "address must be an address or array of addresses")
	}
	set[one] = true
	return set, nil
}

// parseTopicFilter turns the positional topics filter into, per position, the
// set of acceptable hashes (nil = match anything at that position). A log must
// have at least as many topics as the filter has positions.
func parseTopicFilter(topics []json.RawMessage) ([][]common.Hash, error) {
	out := make([][]common.Hash, len(topics))
	for i, raw := range topics {
		if len(raw) == 0 || string(raw) == "null" {
			out[i] = nil // any
			continue
		}
		var many []common.Hash
		if err := json.Unmarshal(raw, &many); err == nil {
			out[i] = many
			continue
		}
		var one common.Hash
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, Err(CodeInvalidParams, "each topic must be null, a hash, or an array of hashes")
		}
		out[i] = []common.Hash{one}
	}
	return out, nil
}

// logMatches applies the address set and the positional topic filter to one log.
func logMatches(l *common.Log, addrs map[common.Address]bool, topics [][]common.Hash) bool {
	if addrs != nil && !addrs[l.Address] {
		return false
	}
	// A filter with N positions requires the log to have at least N topics, and
	// each non-nil position must contain one of the log's topic at that index.
	if len(topics) > len(l.Topics) {
		return false
	}
	for i, allowed := range topics {
		if allowed == nil {
			continue // any
		}
		ok := false
		for _, h := range allowed {
			if l.Topics[i] == h {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
