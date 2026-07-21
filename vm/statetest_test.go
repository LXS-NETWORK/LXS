package vm

import (
	"bytes"
	"embed"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"lxs/common"
)

// A conformance runner in the shape of Ethereum's VMTests / GeneralStateTests:
// load a pre-state, execute code, assert the post-state, return data, and gas.
// The cases live as JSON under testdata/ so official ethereum/tests fixtures
// could be dropped in later with no code change.
//
// Scope: these curated fixtures are in the VMTest format and every expected
// value is independently verifiable (computable arithmetic, EVM wrap-around, a
// published keccak known-answer, storage round-trips). They are not the official
// suite and this does not claim full GeneralStateTests conformance, which needs
// the whole opcode set, RLP transactions, and EIP-2929 access lists (out of
// scope). It proves the VM runs the vector format and an opcode+storage subset
// correctly.

//go:embed testdata/*.json
var fixturesFS embed.FS

type vmFixture struct {
	Exec struct {
		Code    string `json:"code"`
		Data    string `json:"data"`
		Gas     string `json:"gas"`
		Address string `json:"address"`
		Caller  string `json:"caller"`
		Value   string `json:"value"`
	} `json:"exec"`
	Pre  map[string]fixtureAcct `json:"pre"`
	Post map[string]fixtureAcct `json:"post"`
	Out  string                 `json:"out"`
	Gas  string                 `json:"gas"` // expected gas remaining (optional)
}

type fixtureAcct struct {
	Balance string            `json:"balance"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage"`
}

func TestVMStateFixtures(t *testing.T) {
	entries, err := fixturesFS.ReadDir("testdata")
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	ran := 0
	for _, e := range entries {
		raw, err := fixturesFS.ReadFile("testdata/" + e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var suite map[string]vmFixture
		if err := json.Unmarshal(raw, &suite); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for name, tc := range suite {
			ran++
			t.Run(strings.TrimSuffix(e.Name(), ".json")+"/"+name, func(t *testing.T) {
				runVMFixture(t, tc)
			})
		}
	}
	if ran == 0 {
		t.Fatal("no fixtures ran — the embedded testdata is empty")
	}
}

func runVMFixture(t *testing.T, tc vmFixture) {
	st := newMockState()

	// Load the pre-state: balances, code, and storage of every account.
	for addrHex, a := range tc.Pre {
		addr := fxAddr(t, addrHex)
		if a.Balance != "" {
			st.AddBalance(addr, fxBig(t, a.Balance))
		}
		if a.Code != "" {
			st.SetCode(addr, fxBytes(t, a.Code))
		}
		for k, v := range a.Storage {
			st.SetStorage(addr, fxHash(t, k), fxHash(t, v))
		}
	}

	ctx := Context{
		Address: fxAddr(t, tc.Exec.Address),
		Caller:  fxAddr(t, tc.Exec.Caller),
		Value:   fxBig(t, tc.Exec.Value),
		State:   st,
	}
	r := Run(fxBytes(t, tc.Exec.Code), fxBytes(t, tc.Exec.Data), fxU64(t, tc.Exec.Gas), ctx)

	// Return data.
	if tc.Out != "" {
		if want := fxBytes(t, tc.Out); !bytes.Equal(r.Ret, want) {
			t.Fatalf("return data:\n got  %x\n want %x", r.Ret, want)
		}
	}

	// Gas remaining (only meaningful on a non-faulting run).
	if tc.Gas != "" {
		if r.Err != nil {
			t.Fatalf("fixture expects gas left but run faulted: %v", r.Err)
		}
		if want := fxU64(t, tc.Gas); r.GasLeft != want {
			t.Fatalf("gas left = %d, want %d (used %d)", r.GasLeft, want, fxU64(t, tc.Exec.Gas)-r.GasLeft)
		}
	}

	// Post-state.
	for addrHex, a := range tc.Post {
		addr := fxAddr(t, addrHex)
		if a.Balance != "" {
			if got := st.GetBalance(addr); got.Cmp(fxBig(t, a.Balance)) != 0 {
				t.Errorf("account %s balance = %s, want %s", addrHex, got, fxBig(t, a.Balance))
			}
		}
		for k, v := range a.Storage {
			if got := st.GetStorage(addr, fxHash(t, k)); got != fxHash(t, v) {
				t.Errorf("account %s storage[%s] = %x, want %s", addrHex, k, got, v)
			}
		}
	}
}

// --- hex helpers (every fixture value is 0x-prefixed hex) ---

func strip0x(s string) string { return strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X") }

func fxBytes(t *testing.T, s string) []byte {
	t.Helper()
	s = strip0x(s)
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex bytes %q: %v", s, err)
	}
	return b
}

func fxBig(t *testing.T, s string) *big.Int {
	t.Helper()
	s = strip0x(s)
	if s == "" {
		return new(big.Int)
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		t.Fatalf("bad hex int %q", s)
	}
	return v
}

func fxU64(t *testing.T, s string) uint64 {
	t.Helper()
	s = strip0x(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		t.Fatalf("bad hex uint %q: %v", s, err)
	}
	return v
}

func fxAddr(t *testing.T, s string) common.Address {
	t.Helper()
	var a common.Address
	b := fxBytes(t, s)
	if len(b) > len(a) {
		t.Fatalf("address too long: %s", s)
	}
	copy(a[len(a)-len(b):], b) // right-align
	return a
}

func fxHash(t *testing.T, s string) common.Hash {
	t.Helper()
	var h common.Hash
	b := fxBytes(t, s)
	if len(b) > len(h) {
		t.Fatalf("hash too long: %s", s)
	}
	copy(h[len(h)-len(b):], b) // right-align, like a storage key/value word
	return h
}
