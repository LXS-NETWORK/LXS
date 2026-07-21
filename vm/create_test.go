package vm

import (
	"bytes"
	"testing"

	"lxs/common"
)

// childRuntime is a trivial contract that returns the word 0x42, and childInit
// is the constructor that returns that runtime — what CREATE stores.
var (
	childRuntime = []byte{0x60, 0x42, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
	// PUSH10 <runtime> ; PUSH1 0 ; MSTORE ; PUSH1 10 ; PUSH1 22 ; RETURN
	childInit = append(append([]byte{0x69}, childRuntime...),
		0x60, 0x00, 0x52, 0x60, 0x0a, 0x60, 0x16, 0xf3)
)

// factory copies its calldata (the init code) into memory and CREATEs from it,
// returning the new address: the shape of a deployer contract.
var factory = b(
	byte(CALLDATASIZE), byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(CALLDATACOPY),
	byte(CALLDATASIZE), byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(CREATE),
	byte(PUSH1), 0x00, byte(MSTORE), byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN),
)

func addrFromRet(ret []byte) common.Address {
	var a common.Address
	if len(ret) == 32 {
		copy(a[:], ret[12:])
	}
	return a
}

// TestCreateDeploysContract: CREATE runs the init code, stores the runtime it
// returns at the derived address, and that address is then a working contract.
func TestCreateDeploysContract(t *testing.T) {
	state := newMockState()
	self := addr(0xAA)

	r := Run(factory, childInit, 2_000_000, Context{Address: self, State: state})
	if r.Err != nil {
		t.Fatalf("factory execution failed: %v", r.Err)
	}
	got := addrFromRet(r.Ret)

	// Address is keccak(rlp([creator, nonce=0]))[12:].
	want := createAddress(self, 0)
	if got != want {
		t.Fatalf("CREATE address = %x, want %x", got, want)
	}
	// The creator's nonce advanced.
	if state.GetNonce(self) != 1 {
		t.Fatalf("creator nonce = %d, want 1 after a CREATE", state.GetNonce(self))
	}
	// The runtime code is stored, and calling it works.
	if !bytes.Equal(state.GetCode(got), childRuntime) {
		t.Fatalf("deployed code = %x, want %x", state.GetCode(got), childRuntime)
	}
	call := Run(state.GetCode(got), nil, 100_000, Context{Address: got, State: state})
	if call.Err != nil || len(call.Ret) != 32 || call.Ret[31] != 0x42 {
		t.Fatalf("calling the created contract returned %x (err %v), want ..42", call.Ret, call.Err)
	}
}

// TestCreate2DeterministicAddress: CREATE2's address depends on the creator,
// salt, and init code, not the nonce, so it is predictable before deployment.
// Checks the address matches the spec formula.
func TestCreate2DeterministicAddress(t *testing.T) {
	state := newMockState()
	self := addr(0xAA)
	var salt [32]byte
	salt[31] = 0x99

	// CREATE2 factory: copy init, then push salt, size, offset, value, CREATE2.
	code := b(byte(CALLDATASIZE), byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(CALLDATACOPY))
	code = append(code, byte(PUSH32))
	code = append(code, salt[:]...)
	code = append(code, byte(CALLDATASIZE), byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(CREATE2),
		byte(PUSH1), 0x00, byte(MSTORE), byte(PUSH1), 0x20, byte(PUSH1), 0x00, byte(RETURN))

	r := Run(code, childInit, 2_000_000, Context{Address: self, State: state})
	if r.Err != nil {
		t.Fatalf("create2 factory failed: %v", r.Err)
	}
	got := addrFromRet(r.Ret)
	want := create2Address(self, salt, childInit)
	if got != want {
		t.Fatalf("CREATE2 address = %x, want %x (keccak(0xff‖creator‖salt‖keccak(init)))", got, want)
	}
	if !bytes.Equal(state.GetCode(got), childRuntime) {
		t.Fatalf("CREATE2 did not store the runtime code")
	}
}

// TestVMCreateAddressKnownAnswer pins the VM's CREATE derivation against the
// canonical go-ethereum vector: an independent check that does not reuse
// createAddress for its own expectation.
func TestVMCreateAddressKnownAnswer(t *testing.T) {
	sender, err := common.AddressFromHex("0x6ac7ea33f8831ea9dcc53393aaa88b25a785dbf0")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0xcd234a471b72ba2f1ccf0a70fcaba648a5eecd8d", // nonce 0
		"0x343c43a37d37dff08ae8c4a11544c718abb4fcf8", // nonce 1
		"0xf778b86fa74e846c4f0a1fbd1335fe81c00a0c91", // nonce 2
	}
	for nonce, w := range want {
		if got := createAddress(sender, uint64(nonce)).Hex(); got != w {
			t.Errorf("createAddress(sender, %d) = %s, want %s", nonce, got, w)
		}
	}
}

// TestCreateForbiddenInStatic: CREATE is a state change, so it must fault in a
// static frame rather than deploy.
func TestCreateForbiddenInStatic(t *testing.T) {
	state := newMockState()
	r := Run(factory, childInit, 2_000_000, Context{Address: addr(0xAA), State: state, Static: true})
	if r.Err != ErrStaticStateChange {
		t.Fatalf("CREATE in a static context: got %v, want ErrStaticStateChange", r.Err)
	}
}
