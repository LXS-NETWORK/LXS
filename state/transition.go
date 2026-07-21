package state

import (
	"errors"
	"fmt"
	"math/big"

	"lxs/common"
	"lxs/types"
	"lxs/vm"
)

// BaseBlockReward is era-0 issuance before any halving: 50 LXS, 100% to the
// miner. With 4-minute blocks (core.TargetBlockTime) and halving every 1,000,000
// blocks (~7.6 yr/era), total mined ≈ 2 * base * HalvingInterval = 100,000,000
// LXS, ending when the reward reaches 0 (~66 halvings, ~500 yr). Effective max ≈
// 20M genesis + 100M mined = 120M.
var BaseBlockReward = new(big.Int).Mul(big.NewInt(50), big.NewInt(1e18)) // 50 LXS

// HalvingInterval is the block count between reward halvings (50 -> 25 -> ...
// -> 0). The reward sum is a converging geometric series (total ≈ 2 * base *
// interval = 100M), so issuance is finite though the chain runs forever.
//
// When the reward reaches zero, tx fees are the miner's only revenue and security
// becomes fee-based, which is why fees flow to the miner today (see distributeFee).
const HalvingInterval uint64 = 1_000_000

// BlockRewardAt is the issuance at a height: BaseBlockReward >> (height /
// HalvingInterval). Pure and deterministic; every node must derive the same
// reward or the state roots diverge.
func BlockRewardAt(height uint64) *big.Int {
	return new(big.Int).Rsh(BaseBlockReward, uint(height/HalvingInterval))
}

var (
	ErrNonceTooLow       = errors.New("state: nonce too low")
	ErrNonceTooHigh      = errors.New("state: nonce too high")
	ErrInsufficient      = errors.New("state: insufficient balance")
	ErrGasLimit          = errors.New("state: block gas limit exceeded")
	ErrBadStateRoot      = errors.New("state: state root mismatch")
	ErrBadTxRoot         = errors.New("state: tx root mismatch")
	ErrBadReceiptRoot    = errors.New("state: receipt root mismatch")
	ErrBadParent         = errors.New("state: parent hash mismatch")
	ErrBadHeight         = errors.New("state: non-sequential height")
	ErrBadTimestamp      = errors.New("state: non-monotonic timestamp")
	ErrBadGasLimit       = errors.New("state: block gas limit out of bounds vs parent")
	ErrContractCollision = errors.New("state: contract creation over an existing account (EIP-684)")
	ErrBlockPanic        = errors.New("state: panic during block execution")
)

const (
	// GasLimitBoundDivisor bounds per-block gas-limit change (Ethereum-style:
	// |new-parent| < parent/1024). Without it a miner could declare a huge
	// Header.GasLimit and force every node to re-execute an oversized block: a
	// liveness DoS. The honest producer copies the parent's limit (zero delta).
	GasLimitBoundDivisor uint64 = 1024
	// MinBlockGasLimit floors the limit so it cannot be driven toward zero.
	MinBlockGasLimit uint64 = 1_000_000
)

// ApplyTx executes one transaction against the state. Two properties are
// load-bearing:
//
//  1. Determinism: same state + same tx => same result everywhere. No maps, wall
//     clock, randomness, or floats.
//  2. Atomicity: it fully applies or changes nothing; a failed tx leaves no trace.
func ApplyTx(s *State, tx *types.Transaction, coinbase common.Address, blockGasLimit uint64) (gasUsed uint64, status uint64, logs []*common.Log, err error) {
	sender, err := tx.Sender()
	if err != nil {
		return 0, types.ReceiptFailed, nil, err
	}

	acc := s.Get(sender).copy()

	// Nonce: exact match, not >=. Enforces per-account total ordering and makes
	// replay impossible; a tx is valid at one point in an account's history.
	if tx.Nonce < acc.Nonce {
		return 0, types.ReceiptFailed, nil, fmt.Errorf("%w: have %d want %d", ErrNonceTooLow, tx.Nonce, acc.Nonce)
	}
	if tx.Nonce > acc.Nonce {
		return 0, types.ReceiptFailed, nil, fmt.Errorf("%w: have %d want %d", ErrNonceTooHigh, tx.Nonce, acc.Nonce)
	}
	intrinsic := types.IntrinsicGasFor(tx.Data, tx.To == nil)
	if tx.GasLimit < intrinsic {
		return 0, types.ReceiptFailed, nil, fmt.Errorf("%w: gas limit below intrinsic", ErrGasLimit)
	}
	// A tx may not claim more gas than a whole block. Without this a single tx with
	// GasLimit = 2^63 running an infinite loop executes to completion here — hanging every
	// validating node before ApplyBlock's post-loop total-gas check is ever reached (and
	// the panic firewall never fires, because the goroutine is computing, not panicking).
	// Bounding here protects both validation (ApplyBlock) and block building. Mirrors geth's
	// per-tx block GasPool, which rejects an oversize tx before any execution.
	if blockGasLimit > 0 && tx.GasLimit > blockGasLimit {
		return 0, types.ReceiptFailed, nil, fmt.Errorf("%w: tx gas %d exceeds block gas limit %d", ErrGasLimit, tx.GasLimit, blockGasLimit)
	}

	// Charge the worst case up front, refund later. Charging actual gas at the end
	// lets a tx run, drain, then fail to pay: free computation.
	cost := tx.Cost()
	if acc.Balance.Cmp(cost) < 0 {
		return 0, types.ReceiptFailed, nil, fmt.Errorf("%w: have %s want %s", ErrInsufficient, acc.Balance, cost)
	}

	// Gas and nonce commit first and never roll back: a reverted call still pays
	// and advances the nonce, else a failing tx is free and replayable.
	maxFee := new(big.Int).Mul(new(big.Int).SetUint64(tx.GasLimit), tx.GasPrice)
	acc.Balance.Sub(acc.Balance, maxFee)
	acc.Nonce++
	s.set(sender, acc)

	// Record the log count before this tx so its own logs can be sliced out; on
	// revert they truncate back to here and a failed tx reports none.
	logStart := s.LogCount()

	// Everything past here is undone on failure. Snapshot with gas already paid.
	snap := s.Snapshot()

	// Move value out of the sender; the cost check guarantees it covers
	// maxFee + value, so this cannot go negative.
	sacc := s.Get(sender).copy()
	sacc.Balance.Sub(sacc.Balance, tx.Value)
	s.set(sender, sacc)

	gasBudget := tx.GasLimit - intrinsic
	var vmGasUsed uint64
	var vmErr error

	// Block/tx environment every frame sees. Address/Caller/Value are set per
	// frame; the rest is constant for the tx.
	env := vm.Context{
		BlockGasLimit: blockGasLimit, State: s,
		Origin: sender, GasPrice: tx.GasPrice, Coinbase: coinbase,
		BlockNumber: s.bctx.number, Time: s.bctx.time, Difficulty: s.bctx.difficulty,
		ChainID: tx.ChainID,
	}

	switch {
	case tx.To == nil:
		// Contract creation. Address derived from sender and nonce, known before
		// the code runs.
		addr := CreateAddress(sender, tx.Nonce)
		// EIP-684: never deploy over an address that already holds code or a
		// nonce. A collision is near-impossible, but must be structurally so.
		if s.GetNonce(addr) != 0 || len(s.GetCode(addr)) > 0 {
			vmErr, vmGasUsed = ErrContractCollision, gasBudget
			break
		}
		s.Credit(addr, tx.Value)
		ctx := env
		ctx.Address, ctx.Caller, ctx.Value = addr, sender, tx.Value
		res := vm.Run(tx.Data, nil, gasBudget, ctx)
		vmGasUsed = gasBudget - res.GasLeft
		vmErr = res.Err
		if vmErr == nil {
			// EIP-170: reject an over-large contract before storing it.
			if uint64(len(res.Ret)) > vm.MaxCodeSize {
				vmErr, vmGasUsed = vm.ErrMaxCodeSize, gasBudget
			} else {
				// Init code returned the runtime code; pay per byte to store it, a
				// permanent cost on every node.
				codeGas := codeDepositGas * uint64(len(res.Ret))
				if res.GasLeft < codeGas {
					vmErr, vmGasUsed = vm.ErrOutOfGas, gasBudget
				} else {
					vmGasUsed += codeGas
					s.SetCode(addr, res.Ret)
				}
			}
		}

	case len(s.GetCode(*tx.To)) > 0:
		// Contract call. Credit value to the callee before running its code
		// (standard EVM semantics): msg.value sits in its balance during
		// execution, so a value-forwarding contract can send it on. The credit is
		// inside the snapshot above, so a revert unwinds it.
		s.Credit(*tx.To, tx.Value)
		ctx := env
		ctx.Address, ctx.Caller, ctx.Value = *tx.To, sender, tx.Value
		res := vm.Run(s.GetCode(*tx.To), tx.Data, gasBudget, ctx)
		vmGasUsed = gasBudget - res.GasLeft
		vmErr = res.Err

	case *tx.To == common.BurnAddress:
		// Recognised burn. Value sent to the burn address is destroyed, not
		// credited: it left the sender above and folds into the consensus burn
		// total instead of landing in an account, so the address never holds a
		// balance and supply actually decreases. Transaction-level only; a
		// contract-internal send here is out of scope.
		s.Burn(tx.Value)

	default:
		// Plain transfer to a codeless account. No VM, intrinsic gas only.
		s.Credit(*tx.To, tx.Value)
	}

	status = types.ReceiptSuccess
	if vmErr != nil {
		// Undo the value move and every write; gas and nonce stay committed.
		s.RevertToSnapshot(snap)
		status = types.ReceiptFailed
		// A revert refunds unspent gas; any other fault burns the lot.
		if !errors.Is(vmErr, vm.ErrReverted) {
			vmGasUsed = gasBudget
		}
	} else {
		// Committed: drop the savepoints.
		s.DiscardSnapshots()
	}

	gasUsed = intrinsic + vmGasUsed
	spent := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), tx.GasPrice)
	if refund := new(big.Int).Sub(maxFee, spent); refund.Sign() > 0 {
		s.Credit(sender, refund)
	}
	distributeFee(s, coinbase, spent)

	// LogsSince returns this tx's events on success, nil on failure (already
	// truncated to logStart).
	return gasUsed, status, s.LogsSince(logStart), nil
}

// Call runs to's code with data as a read-only probe (eth_call): no signature,
// nonce, gas, or commit. The caller passes a throwaway state copy, so any SSTORE
// is discarded. Returns the code's RETURN data or the revert/fault error (a
// revert is a legitimate answer, often carrying the reason).
func Call(s *State, from, to common.Address, data []byte, gas uint64) ([]byte, error) {
	res := vm.Run(s.GetCode(to), data, gas, vm.Context{
		Address: to, Caller: from, Value: new(big.Int), State: s,
	})
	return res.Ret, res.Err
}

// EstimateGas returns the smallest gas limit at which a message actually executes
// to completion (eth_estimateGas), found by binary search. A revert is surfaced as
// an error, matching eth_call, so wallets can warn instead of submitting a doomed
// tx.
//
// A single run at gasCap is NOT a safe estimate. The EIP-150 63/64 rule forwards
// gas to a sub-call as a fraction of what remains, so a nested call that just barely
// succeeds with the huge gasCap budget can run out when the tx is later submitted
// with the tighter measured limit — the very reason MetaMask "buy"/"sell" through a
// bonding-curve router with a nested transfer would intermittently fail out-of-gas.
// The binary search returns a limit proven to run at that exact limit, not merely
// under a fatter budget.
//
// probe() reproduces ApplyTx's budget semantics exactly (creation runs the init
// code and pays the per-byte code-deposit out of the same budget, so a deploy is
// not mis-estimated), running against the caller's throwaway state copy and undoing
// every write via a savepoint so probes don't compound.
func EstimateGas(s *State, from common.Address, to *common.Address, data []byte, value *big.Int, gasCap uint64) (uint64, error) {
	intrinsic := types.IntrinsicGasFor(data, to == nil)
	if value == nil {
		value = new(big.Int)
	}

	// probe reports whether the message completes without a fault at the given total
	// gas limit. ok is true only on clean completion; hardErr is set for a
	// gas-insensitive failure (a revert or a bad-opcode fault) — one that no extra
	// gas can fix — so the search can stop and surface it. Out-of-gas returns
	// (false, nil): retry higher.
	probe := func(gasLimit uint64) (ok bool, hardErr error) {
		if gasLimit < intrinsic {
			return false, nil // cannot even cover intrinsic; a higher limit might
		}
		budget := gasLimit - intrinsic
		snap := s.Snapshot()
		defer s.RevertToSnapshot(snap)

		ctx := vm.Context{Caller: from, Value: value, State: s}
		var res vm.Result
		if to == nil {
			ctx.Address = CreateAddress(from, s.GetNonce(from))
			// Credit the call value to the new contract before running its init code,
			// exactly as ApplyTx does. Without this a factory/router that forwards its
			// received value onward hits opCall's balance gate (vm/call.go), the inner
			// CALL fail-fasts, and the probe reports a revert the real (value-credited)
			// tx never has — the value>0 buy/sell path this estimator exists for. The
			// credit is inside the per-probe snapshot, so it unwinds.
			s.Credit(ctx.Address, value)
			res = vm.Run(data, nil, budget, ctx)
			if res.Err != nil {
				return false, ternErr(res.Err == vm.ErrOutOfGas, res.Err)
			}
			if uint64(len(res.Ret)) > vm.MaxCodeSize {
				return false, vm.ErrMaxCodeSize // gas-insensitive: too big at any budget
			}
			if res.GasLeft < codeDepositGas*uint64(len(res.Ret)) {
				return false, nil // deposit didn't fit; a higher budget will
			}
			return true, nil
		}
		if len(s.GetCode(*to)) == 0 {
			return true, nil // plain transfer / burn: no VM, intrinsic covers it
		}
		ctx.Address = *to
		// Credit the value to the callee before running its code, as ApplyTx does, so
		// a value-forwarding contract has the balance to send it on (else the inner
		// CALL fail-fasts and the probe fabricates a revert). Inside the snapshot.
		s.Credit(*to, value)
		res = vm.Run(s.GetCode(*to), data, budget, ctx)
		if res.Err != nil {
			return false, ternErr(res.Err == vm.ErrOutOfGas, res.Err)
		}
		return true, nil
	}

	// The whole message must be executable at the allowance ceiling; if it faults
	// there, no achievable limit helps. Surface a revert/fault error; a bare
	// out-of-gas means the message needs more than gasCap.
	ok, hardErr := probe(gasCap)
	if hardErr != nil {
		return 0, hardErr
	}
	if !ok {
		return 0, vm.ErrOutOfGas // gas required exceeds the allowance
	}

	// Binary search for the least limit that still executes. Invariant: probe(lo) is
	// a fault (lo starts below intrinsic) and probe(hi) succeeds.
	lo, hi := intrinsic-1, gasCap
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		ok, hardErr := probe(mid)
		if hardErr != nil {
			// A gas-sensitive branch revealed a fault only at this budget; treat as
			// "need more" and keep the proven-good hi.
			lo = mid
			continue
		}
		if ok {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, nil
}

// ternErr returns err when cond is false (a gas-insensitive fault to surface), nil
// when cond is true (an out-of-gas the search should retry). Keeps probe's returns
// terse.
func ternErr(isOutOfGas bool, err error) error {
	if isOutOfGas {
		return nil
	}
	return err
}

// codeDepositGas is charged per byte of deployed code, which lives on every node
// forever (Ethereum's 200/byte).
const codeDepositGas uint64 = 200

// CreateAddress derives a contract address from its creator and the creating
// tx's nonce, deterministically. Ethereum's exact derivation:
// keccak256(rlp([sender, nonce]))[12:]. External tooling predicts the address
// before deployment and pre-funds it, so a one-byte difference would strand funds
// at an uncontrolled address.
func CreateAddress(sender common.Address, nonce uint64) common.Address {
	h := common.Keccak256(rlpList(rlpBytes(sender.Bytes()), rlpUint(nonce)))
	var addr common.Address
	copy(addr[:], h[12:]) // last 20 bytes of the 32-byte hash
	return addr
}

// TreasuryRewardBasisPoints is the share of each block reward routed to
// TreasuryRewardAddress instead of the proposer (2000 = 20%). A protocol constant
// like FeeBurnBasisPoints: every node must agree or reward arithmetic and roots
// diverge. 0 (default) means the proposer keeps 100%.
var TreasuryRewardBasisPoints int64 = 0

// TreasuryRewardAddress receives the TreasuryRewardBasisPoints share. Zero
// (default) disables the split regardless of bps: the split is on only when a real
// address and a positive share are both set, so a half-config never silently burns
// the reward to the zero address.
var TreasuryRewardAddress common.Address

// CreditBlockReward mints one block's height-adjusted issuance and distributes
// it: 100% to the proposer by default, or a treasury/proposer split when
// configured. One implementation runs on both sides of consensus (producer and
// validator), so their roots match.
//
// The proposer's share is derived as the remainder (reward - treasuryCut), not
// computed independently: two divisions would each round down and leak lux the
// conservation invariant rejects. The split redirects an issued reward, minting
// nothing extra.
func CreditBlockReward(s *State, miner common.Address, height uint64) {
	reward := BlockRewardAt(height)
	if TreasuryRewardBasisPoints > 0 && TreasuryRewardAddress != (common.Address{}) {
		treasuryCut := new(big.Int).Div(
			new(big.Int).Mul(reward, big.NewInt(TreasuryRewardBasisPoints)),
			big.NewInt(10000),
		)
		if TreasuryRewardAddress == common.BurnAddress {
			// Auto-burn: the cut is destroyed, folding into the consensus burn
			// total like a recognised burn tx, not credited. Crediting the burn
			// address would park coins there without counting them as burned.
			s.Burn(treasuryCut)
		} else {
			s.Credit(TreasuryRewardAddress, treasuryCut)
		}
		reward = new(big.Int).Sub(reward, treasuryCut)
	}
	s.Credit(miner, reward)
}

// ApplyBlock validates and executes a full block against a parent state. Works on
// a copy, returned only on full success: a block is all or nothing.
func ApplyBlock(parent *State, b *types.Block, parentHeader *types.Header) (next *State, receipts []*types.Receipt, err error) {
	// Panic firewall. Block execution runs untrusted input (peer bytecode/calldata)
	// through the VM; an unrecovered panic in this goroutine would kill the whole
	// process, and since a gossiped poison block reaches every node — and is re-offered
	// after a restart — one crafted block could crash-loop the entire network. Work
	// happens on parent.Copy() (below), so parent is untouched; recovering to a rejected
	// block is safe and turns a network-wide kill into one dropped block.
	defer func() {
		if r := recover(); r != nil {
			next, receipts, err = nil, nil, fmt.Errorf("%w: %v", ErrBlockPanic, r)
		}
	}()
	if !b.VerifyTxRoot() {
		return nil, nil, ErrBadTxRoot
	}
	// A non-genesis block must carry its parent header; the checks below are the
	// only thing binding it to the chain. Only genesis (height 0) may have no
	// parent, so a nil parent otherwise is a wiring bug that skips every check.
	if parentHeader == nil && b.Header.Height > 0 {
		return nil, nil, ErrBadParent
	}
	if parentHeader != nil {
		if b.Header.ParentHash != parentHeader.Hash() {
			return nil, nil, ErrBadParent
		}
		if b.Header.Height != parentHeader.Height+1 {
			return nil, nil, ErrBadHeight
		}
		if b.Header.Timestamp <= parentHeader.Timestamp {
			return nil, nil, ErrBadTimestamp
		}
		// Bound the gas limit vs the parent (±parent/1024) with a floor, so a miner
		// cannot declare an arbitrary Header.GasLimit and force the network to
		// re-execute an oversized block. In the deterministic transition, so every
		// node agrees.
		lim, plim := b.Header.GasLimit, parentHeader.GasLimit
		diff := lim - plim
		if lim < plim {
			diff = plim - lim
		}
		if lim < MinBlockGasLimit || diff >= plim/GasLimitBoundDivisor {
			return nil, nil, ErrBadGasLimit
		}
	}

	next = parent.Copy()
	// From here Touched() means changed by this block, which persistence needs to
	// write a diff instead of the whole world.
	next.ClearTouched()
	// The same block environment the producer used, from this block's header, so
	// NUMBER/TIMESTAMP/DIFFICULTY and the root match on every node. Timestamp ms -> s.
	next.SetBlockContext(b.Header.Height, uint64(b.Header.Timestamp)/1000, b.Header.Difficulty)

	var totalGas uint64
	receipts = make([]*types.Receipt, 0, len(b.Txs))

	for i, tx := range b.Txs {
		used, status, logs, err := ApplyTx(next, tx, b.Header.Proposer, b.Header.GasLimit)
		if err != nil {
			// An invalid tx (bad nonce, unaffordable) invalidates the whole block
			// rather than being skipped, else "which txs were skipped" becomes a
			// consensus question. A reverted call is different: it returns no
			// error, is included, consumes gas, and fails only in its receipt Status.
			return nil, nil, fmt.Errorf("tx %d (%s): %w", i, tx.Hash().Hex(), err)
		}
		totalGas += used
		if totalGas > b.Header.GasLimit {
			return nil, nil, ErrGasLimit
		}
		receipts = append(receipts, &types.Receipt{
			Status:            status,
			GasUsed:           used,
			CumulativeGasUsed: totalGas,
			Logs:              logs,
		})
	}

	if totalGas != b.Header.GasUsed {
		return nil, nil, fmt.Errorf("state: gas used mismatch: computed %d header %d", totalGas, b.Header.GasUsed)
	}

	if root := types.ReceiptRoot(receipts); root != b.Header.ReceiptRoot {
		return nil, nil, fmt.Errorf("%w: computed %s header %s", ErrBadReceiptRoot, root.Hex(), b.Header.ReceiptRoot.Hex())
	}

	// Issuance last, after fees are paid by distributeFee, so the miner earns
	// reward plus fees (fee income keeps mining worthwhile as halving drives the
	// reward toward zero). A validator disagreeing about who mined or how much was
	// issued computes a different root below and rejects the block.
	CreditBlockReward(next, b.Header.Proposer, b.Header.Height)

	// Proof of correct execution: the independently recomputed world-state root
	// matches the proposer's claim.
	if root := next.Root(); root != b.Header.StateRoot {
		return nil, nil, fmt.Errorf("%w: computed %s header %s", ErrBadStateRoot, root.Hex(), b.Header.StateRoot.Hex())
	}

	return next, receipts, nil
}

// FeeBurnBasisPoints is the fraction of every tx fee burned instead of paid to
// the proposer (2000 = 20%). The burn scales with fee activity, so once it
// outpaces issuance net supply shrinks.
//
// Deliberately not 100%: burn a slice of the fee (this), tip the rest to the
// proposer (below), and issue the block reward on top (CreditBlockReward). The
// tip plus reward is the security budget; a 100% burn would take it with the
// reward once halving reaches zero.
var FeeBurnBasisPoints = uint64(2000)

// distributeFee routes one tx's gas fee: burn a fixed slice, pay the remainder to
// the proposer. The block reward is issued separately, so the proposer earns
// reward + tip.
func distributeFee(s *State, proposer common.Address, fee *big.Int) {
	// Burn rounds down; tip = fee - burn takes the remainder, so burn + tip == fee
	// exactly and the proposer absorbs any rounding dust.
	burn := new(big.Int).Mul(fee, new(big.Int).SetUint64(FeeBurnBasisPoints))
	burn.Div(burn, big.NewInt(10000))
	tip := new(big.Int).Sub(fee, burn)

	s.Burn(burn)            // destroyed, folds into the burn total and the root
	s.Credit(proposer, tip) // proposer's priority income
}
