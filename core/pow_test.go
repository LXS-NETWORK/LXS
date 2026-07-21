package core

import (
	"errors"
	"math/big"
	"testing"

	"lxs/types"
)

func TestPoWTargetShrinksWithDifficulty(t *testing.T) {
	easy := powTarget(1)
	hard := powTarget(1_000_000)
	if easy.Cmp(hard) <= 0 {
		t.Fatal("higher difficulty must mean a smaller target")
	}
	if powTarget(1).Cmp(maxTarget) != 0 {
		t.Fatal("difficulty 1 must open the whole hash space")
	}
}

// A mined header satisfies its target; nudging the nonce off the solution breaks
// it. The property PoW rests on.
func TestMinedHeaderSatisfiesPoWTamperedDoesNot(t *testing.T) {
	h := &types.Header{Difficulty: 4096, Timestamp: 1}
	if !mine(h, nil) {
		t.Fatal("mining a difficulty-4096 header should succeed quickly")
	}
	if !satisfiesPoW(h) {
		t.Fatal("a freshly mined header must satisfy its own target")
	}

	solved := h.Nonce
	// Find a nonce that fails; at difficulty 4096 the vast majority do.
	found := false
	for n := solved + 1; n < solved+1_000_000; n++ {
		h.Nonce = n
		h.InvalidateHash()
		if !satisfiesPoW(h) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected some nonce to fail the target")
	}
	if satisfiesPoW(h) {
		t.Fatal("a header with a losing nonce must not satisfy the target")
	}
}

func TestInsertRejectsBadPoW(t *testing.T) {
	alice := newKey(t)
	g := testGenesis(alice.Address())
	g.Difficulty = 4096
	bc := NewMemBlockchain(g)

	blk := newBranch(t, bc, bc.Head()).next() // mined, valid

	// Knock the nonce off its solution while leaving Difficulty intact, so the
	// rejection is specifically about work.
	h := blk.Header
	solved := h.Nonce
	for n := solved + 1; ; n++ {
		h.Nonce = n
		h.InvalidateHash()
		if !satisfiesPoW(h) {
			break
		}
	}

	if err := bc.InsertBlock(blk); !errors.Is(err, ErrBadPoW) {
		t.Fatalf("insert with a losing nonce: got %v, want ErrBadPoW", err)
	}
}

func TestInsertRejectsWrongDifficulty(t *testing.T) {
	alice := newKey(t)
	g := testGenesis(alice.Address())
	g.Difficulty = 4096
	bc := NewMemBlockchain(g)

	blk := newBranch(t, bc, bc.Head()).next()
	// Claim a difficulty the parent does not derive. The difficulty gate runs before
	// the PoW gate, so this is refused for the right reason.
	blk.Header.Difficulty += 1000
	blk.Header.InvalidateHash()

	if err := bc.InsertBlock(blk); !errors.Is(err, ErrBadDifficulty) {
		t.Fatalf("insert with forged difficulty: got %v, want ErrBadDifficulty", err)
	}
}

// The heaviest chain wins on work, not height: a short heavy chain beats a tall
// light one.
func TestHeaviestChainPrefersWorkOverHeight(t *testing.T) {
	fc := HeaviestChain{}
	tall := &Tip{&types.Header{Height: 100}, big.NewInt(10)}
	short := &Tip{&types.Header{Height: 2}, big.NewInt(20)}

	if !fc.Better(short, tall) {
		t.Fatal("more accumulated work must win regardless of height")
	}
	if fc.Better(tall, short) {
		t.Fatal("less work must not replace more")
	}
}

// VerifyHeaderPoW is the sync pre-check's work barrier: it accepts a header whose nonce
// satisfies the difficulty it claims (>= the floor), and rejects a sub-floor difficulty or
// an unmined header. The exact LWMA difficulty is re-derived authoritatively in InsertBlock.
func TestVerifyHeaderPoW(t *testing.T) {
	// A header at difficulty 1 (whole hash space): nonce 0 satisfies it.
	good := &types.Header{Height: 1, Timestamp: 1_000_000, Difficulty: 1}
	if err := VerifyHeaderPoW(good); err != nil {
		t.Fatalf("a valid header was rejected: %v", err)
	}
	// Below the floor (an attacker claiming difficulty 0 for free PoW).
	below := &types.Header{Height: 1, Timestamp: 1_000_000, Difficulty: 0}
	if err := VerifyHeaderPoW(below); err == nil {
		t.Fatal("a header below the difficulty floor must be rejected")
	}
	// Correctly-claimed but unmined: at a high difficulty, nonce 0 does not satisfy PoW.
	unmined := &types.Header{Height: 1, Timestamp: 1_000_000, Difficulty: 1 << 60}
	if err := VerifyHeaderPoW(unmined); err == nil {
		t.Fatal("a header claiming a high difficulty its nonce does not meet must be rejected")
	}
}

// mine must abort (return false) when its stop channel is closed, so the producer
// can cancel an in-progress nonce search once a competing block replaces its parent.
func TestMineAbortsOnClosedStop(t *testing.T) {
	h := &types.Header{Difficulty: 1 << 40} // hard enough that a nonce is not found at once
	stop := make(chan struct{})
	close(stop)
	if mine(h, stop) {
		t.Fatal("mine must return false (aborted) when stop is already closed")
	}
}
