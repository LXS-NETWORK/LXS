package main

import (
	"fmt"
	"math/big"

	"lxs/common"
	"lxs/core"
	"lxs/crypto"
	"lxs/mempool"
	"lxs/state"
	"lxs/types"
)

// reorgDemo shows a chain reorganising with the indexes following it. It builds
// two competing branches by hand — the Producer cannot, since a node only builds
// on its own head.
func reorgDemo() error {
	alice, _ := crypto.GenerateKey()
	bob, _ := crypto.GenerateKey()

	g := &core.Genesis{
		ChainID: 1337, Timestamp: 1700000000000, GasLimit: 30_000_000,
		Alloc: map[common.Address]*core.BigStr{alice.Address(): {Int: big.NewInt(1_000_000)}},
	}
	bc := core.NewMemBlockchain(g)
	pool := mempool.New(1024)
	core.BindMempool(bc, pool)

	genesis := bc.Head()
	fmt.Printf("fork choice  %s\n", bc.ForkChoice().Name())
	fmt.Printf("genesis      %s\n\n", short(bc.Head().Hash().Hex()))

	// Alice pays Bob 5000 on branch A.
	txA := types.NewTransaction(1337, 0, bob.Address(), big.NewInt(5000), types.IntrinsicGas, big.NewInt(1), nil)
	txA.Sign(alice)
	pool.Add(txA, 1337)

	a1 := forge(bc, genesis, txA)
	if err := bc.InsertBlock(a1); err != nil {
		return err
	}
	pool.Remove(a1.Txs)

	fmt.Printf("BRANCH A\n")
	fmt.Printf("  block 1    %s  (contains alice -> bob 5000)\n", short(a1.Hash().Hex()))
	fmt.Printf("  head       %s\n", short(bc.Head().Hash().Hex()))
	fmt.Printf("  bob        %s\n", bc.StateSnapshot().Balance(bob.Address()))
	_, loc, err := bc.TxByHash(txA.Hash())
	if err == nil {
		fmt.Printf("  tx status  MINED in block %d\n", loc.BlockHeight)
	}
	fmt.Printf("  mempool    %d pending\n\n", pool.Len())

	// Branch B forks from genesis and grows taller, without the tx.
	b1 := forge(bc, genesis)
	if err := bc.InsertBlock(b1); err != nil {
		return err
	}
	fmt.Printf("BRANCH B arrives\n")
	fmt.Printf("  block 1'   %s  (empty, height 1 — does not win yet)\n", short(b1.Hash().Hex()))
	fmt.Printf("  head       %s  (unchanged, or tie-broken by hash)\n\n", short(bc.Head().Hash().Hex()))

	b2 := forge(bc, b1)
	if err := bc.InsertBlock(b2); err != nil {
		return err
	}

	fmt.Printf("BRANCH B extends to height 2 — REORG\n")
	fmt.Printf("  block 2'   %s\n", short(b2.Hash().Hex()))
	fmt.Printf("  head       %s\n", short(bc.Head().Hash().Hex()))
	fmt.Printf("  a1 canonical? %v  (still in the tree: %v)\n", bc.IsCanonical(a1.Hash()), bc.HasBlock(a1.Hash()))
	fmt.Printf("  bob        %s  (the payment was undone)\n", bc.StateSnapshot().Balance(bob.Address()))

	if _, _, err := bc.TxByHash(txA.Hash()); err != nil {
		fmt.Printf("  tx status  NOT MINED — the index was unwound correctly\n")
	} else {
		fmt.Printf("  tx status  STILL MINED — BUG: the node is lying\n")
	}
	fmt.Printf("  mempool    %d pending  (the tx came back — it was not destroyed)\n", pool.Len())

	total := new(big.Int)
	for _, acc := range bc.StateSnapshot().Accounts() {
		total.Add(total, acc.Balance)
	}
	fmt.Printf("\n  supply     %s (must still be 1000000)\n", total)
	return nil
}

// forge builds a valid block on an arbitrary parent.
func forge(bc *core.Blockchain, parent *types.Block, txs ...*types.Transaction) *types.Block {
	st, err := bc.StateAt(parent.Hash())
	if err != nil {
		panic(err)
	}
	k, _ := crypto.GenerateKey()
	proposer := k.Address()

	header := &types.Header{
		ParentHash: parent.Hash(),
		Height:     parent.Height() + 1,
		Timestamp:  parent.Header.Timestamp + 1000,
		GasLimit:   parent.Header.GasLimit,
		Proposer:   proposer,
	}
	var gasUsed uint64
	receipts := make([]*types.Receipt, 0, len(txs))
	for _, tx := range txs {
		used, status, logs, err := state.ApplyTx(st, tx, proposer, header.GasLimit)
		if err != nil {
			panic(err)
		}
		gasUsed += used
		receipts = append(receipts, &types.Receipt{
			Status: status, GasUsed: used, CumulativeGasUsed: gasUsed, Logs: logs,
		})
	}
	header.TxRoot = types.TxRoot(txs)
	header.ReceiptRoot = types.ReceiptRoot(receipts)
	header.StateRoot = st.Root()
	header.GasUsed = gasUsed
	return &types.Block{Header: header, Txs: txs}
}

func short(s string) string {
	if len(s) < 14 {
		return s
	}
	return s[:10] + ".." + s[len(s)-4:]
}
