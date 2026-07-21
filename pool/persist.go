package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"lxs/common"
)

// The pool's ledger on disk. Balances are money the pool OWES workers; pending
// blocks are rewards not yet mature. Losing this file after a restart would
// silently zero worker balances, so it is written atomically (tmp+rename — a
// crash mid-write leaves the old ledger, never a truncated one) on every
// balance-affecting event, not on a timer.
type persistState struct {
	Balances    map[string]string `json:"balances"` // addr -> wei
	Pending     []persistBlock    `json:"pending"`
	TotalShares uint64            `json:"totalShares"`
	TotalBlocks uint64            `json:"totalBlocks"`
	TotalOrphan uint64            `json:"totalOrphans"`
	TotalPaid   string            `json:"totalPaidWei"`
}

type persistBlock struct {
	Height  uint64            `json:"height"`
	Hash    string            `json:"hash"`
	Payouts map[string]string `json:"payouts"`
}

func (s *Server) saveLocked() {
	if s.cfg.StatePath == "" {
		return
	}
	ps := persistState{
		Balances:    map[string]string{},
		TotalShares: s.totalShares,
		TotalBlocks: s.totalBlocks,
		TotalOrphan: s.totalOrphans,
		TotalPaid:   s.totalPaid.String(),
	}
	for addr, bal := range s.balances {
		ps.Balances[addr.Hex()] = bal.String()
	}
	for _, fb := range s.pending {
		pb := persistBlock{Height: fb.Height, Hash: fb.Hash.Hex(), Payouts: map[string]string{}}
		for addr, amt := range fb.Payouts {
			pb.Payouts[addr.Hex()] = amt.String()
		}
		ps.Pending = append(ps.Pending, pb)
	}
	b, err := json.MarshalIndent(ps, "", " ")
	if err != nil {
		s.logf("pool: marshal ledger: %v", err)
		return
	}
	tmp := s.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, s.cfg.StatePath)
		if err != nil {
			s.logf("pool: LEDGER WRITE FAILED (worker balances at risk on restart): %v", err)
		}
	} else {
		s.logf("pool: LEDGER WRITE FAILED (worker balances at risk on restart): %v", err)
	}
}

func (s *Server) load() error {
	if s.cfg.StatePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.StatePath), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(s.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first run
	}
	if err != nil {
		return err
	}
	var ps persistState
	if err := json.Unmarshal(b, &ps); err != nil {
		// A corrupt ledger must stop the pool, not silently start from zero —
		// zeroing balances is exactly the failure persistence exists to prevent.
		return fmt.Errorf("corrupt ledger %s (fix or move it): %w", s.cfg.StatePath, err)
	}
	for hex, wei := range ps.Balances {
		addr, err := common.AddressFromHex(hex)
		if err != nil {
			return fmt.Errorf("ledger: bad address %q", hex)
		}
		v, ok := new(big.Int).SetString(wei, 10)
		if !ok {
			return fmt.Errorf("ledger: bad balance %q", wei)
		}
		s.balances[addr] = v
	}
	for _, pb := range ps.Pending {
		h, err := common.HashFromHex(pb.Hash)
		if err != nil {
			return fmt.Errorf("ledger: bad block hash %q", pb.Hash)
		}
		fb := &foundBlock{Height: pb.Height, Hash: h, Payouts: map[common.Address]*big.Int{}}
		for hex, wei := range pb.Payouts {
			addr, err := common.AddressFromHex(hex)
			if err != nil {
				return fmt.Errorf("ledger: bad payout address %q", hex)
			}
			v, ok := new(big.Int).SetString(wei, 10)
			if !ok {
				return fmt.Errorf("ledger: bad payout amount %q", wei)
			}
			fb.Payouts[addr] = v
		}
		s.pending = append(s.pending, fb)
	}
	s.totalShares = ps.TotalShares
	s.totalBlocks = ps.TotalBlocks
	s.totalOrphans = ps.TotalOrphan
	if ps.TotalPaid != "" {
		if v, ok := new(big.Int).SetString(ps.TotalPaid, 10); ok {
			s.totalPaid = v
		}
	}
	return nil
}
