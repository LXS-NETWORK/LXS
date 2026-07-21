package core

import (
	"errors"
	"math/rand"
	"testing"

	"lxs/common"
	"lxs/types"
)

// Deterministic, seeded simulation of a network under a hostile delivery layer.
// Multiple nodes mine competing blocks; messages are dropped, duplicated, and
// reordered. The property is consensus safety: once the honest nodes have seen the
// same blocks they must all choose the same head, or the network has silently
// partitioned. Everything is deterministic (fixed proposer addresses, parent-derived
// timestamps, seeded PRNG), so a failure is reproducible from its seed.
func TestNetworkConvergesUnderAdversarialDelivery(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 7, 42} {
		seed := seed
		t.Run("seed_"+itoa(seed), func(t *testing.T) {
			runConvergence(t, seed)
		})
	}
}

func runConvergence(t *testing.T, seed int64) {
	const (
		nodes  = 5
		rounds = 500
		pMine  = 0.40 // chance a step mines rather than delivers
		pDrop  = 0.25 // chance a broadcast copy is lost in flight
		pDup   = 0.15 // chance a delivered copy is duplicated
	)

	g := testGenesis()
	chains := make([]*Blockchain, nodes)
	proposers := make([]common.Address, nodes)
	for i := range chains {
		chains[i] = NewMemBlockchain(g)
		proposers[i] = common.Address{0x10 + byte(i)} // fixed => reproducible
	}

	r := rand.New(rand.NewSource(seed))

	type msg struct {
		to  int
		blk *types.Block
	}
	var queue []msg
	var produced []*types.Block // registry, for the flush pass

	mineOn := func(i int) *types.Block {
		br := newBranch(t, chains[i], chains[i].Head())
		br.proposer = proposers[i]
		return br.next() // deterministic block on node i's current head
	}
	broadcast := func(from int, blk *types.Block) {
		produced = append(produced, blk)
		for j := 0; j < nodes; j++ {
			if j == from {
				continue
			}
			if r.Float64() < pDrop {
				continue // lost in flight
			}
			queue = append(queue, msg{j, blk})
			if r.Float64() < pDup {
				queue = append(queue, msg{j, blk}) // arrives twice
			}
		}
	}
	deliver := func(m msg) {
		bc := chains[m.to]
		if bc.HasBlock(m.blk.Hash()) {
			return // gossip dedup: cheap check first, like the real path
		}
		err := bc.InsertBlock(m.blk)
		if errors.Is(err, ErrUnknownParent) {
			queue = append(queue, m) // orphan: retry once its parent shows up
			return
		}
		if err != nil {
			t.Fatalf("seed %d: unexpected insert error: %v", seed, err)
		}
	}

	for step := 0; step < rounds; step++ {
		if len(queue) == 0 || r.Float64() < pMine {
			i := r.Intn(nodes)
			blk := mineOn(i)
			if err := chains[i].InsertBlock(blk); err != nil {
				t.Fatalf("seed %d: a node could not insert its OWN block: %v", seed, err)
			}
			broadcast(i, blk)
		} else {
			k := r.Intn(len(queue)) // random pick => reordering
			m := queue[k]
			queue = append(queue[:k], queue[k+1:]...)
			deliver(m)
		}
	}

	// Flush: model the sync layer backfilling every gap by re-offering every produced
	// block to every node until a full pass changes nothing.
	for pass := 0; ; pass++ {
		if pass > len(produced)+2 {
			t.Fatalf("seed %d: flush did not reach a fixpoint — a block's parent is missing", seed)
		}
		progress := false
		for _, blk := range produced {
			for j := 0; j < nodes; j++ {
				if chains[j].HasBlock(blk.Hash()) {
					continue
				}
				err := chains[j].InsertBlock(blk)
				switch {
				case err == nil:
					progress = true
				case errors.Is(err, ErrUnknownParent):
					// parent not here yet; a later pass gets it
				default:
					t.Fatalf("seed %d: flush insert error: %v", seed, err)
				}
			}
		}
		if !progress {
			break
		}
	}

	// Safety: every node now holds the same blocks, so the total, deterministic fork
	// choice must put them all on the same head.
	head0 := chains[0].Head().Hash()
	for j := 1; j < nodes; j++ {
		if h := chains[j].Head().Hash(); h != head0 {
			t.Fatalf("seed %d: node %d head %s != node 0 head %s — NETWORK PARTITIONED",
				seed, j, h.Hex(), head0.Hex())
		}
	}

	// Liveness sanity: the chain actually advanced.
	if chains[0].Head().Height() == 0 {
		t.Fatalf("seed %d: no block ever took hold", seed)
	}
	t.Logf("seed %d: %d nodes converged on height %d (%d blocks produced)",
		seed, nodes, chains[0].Head().Height(), len(produced))
}

// itoa avoids pulling in strconv for a subtest name.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
