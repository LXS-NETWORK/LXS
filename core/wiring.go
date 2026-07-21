package core

import "lxs/mempool"

// BindMempool wires reorg handling between the chain and the pool.
//
// Order is load-bearing: re-inject the dropped branch first, then remove the added
// branch. A tx present in both branches must end up removed; the reverse order
// re-injects a tx still mined on the new canonical chain, whose nonce is already
// spent, so it is dropped every block and the pool never drains.
func BindMempool(bc *Blockchain, pool *mempool.Mempool) {
	bc.SetReorgHook(func(r *Reorg) {
		for _, blk := range r.Dropped {
			pool.Reinject(blk.Txs, bc.ChainID())
		}
		for _, blk := range r.Added {
			pool.Remove(blk.Txs)
		}
	})
}
