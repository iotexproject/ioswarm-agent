package main

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/holiman/uint256"
)

func TestMemStateDB_BalanceOps(t *testing.T) {
	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000", // 1 ETH
			Nonce:   5,
		},
	}
	db := NewMemStateDB(task)
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")

	bal := db.GetBalance(addr)
	expected := uint256.MustFromDecimal("1000000000000000000")
	if bal.Cmp(expected) != 0 {
		t.Fatalf("expected balance %s, got %s", expected, bal)
	}

	db.AddBalance(addr, uint256.NewInt(500), tracing.BalanceChangeUnspecified)
	bal = db.GetBalance(addr)
	expected = uint256.MustFromDecimal("1000000000000000500")
	if bal.Cmp(expected) != 0 {
		t.Fatalf("after add: expected %s, got %s", expected, bal)
	}

	db.SubBalance(addr, uint256.NewInt(200), tracing.BalanceChangeUnspecified)
	bal = db.GetBalance(addr)
	expected = uint256.MustFromDecimal("1000000000000000300")
	if bal.Cmp(expected) != 0 {
		t.Fatalf("after sub: expected %s, got %s", expected, bal)
	}

	if db.GetNonce(addr) != 5 {
		t.Fatalf("expected nonce 5, got %d", db.GetNonce(addr))
	}
}

func TestMemStateDB_StorageReadWrite(t *testing.T) {
	task := &taskPackage{}
	db := NewMemStateDB(task)
	addr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	slot := common.HexToHash("0x01")
	val := common.HexToHash("0x42")

	// Initially empty
	if got := db.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("expected empty, got %s", got.Hex())
	}

	db.SetState(addr, slot, val)
	if got := db.GetState(addr, slot); got != val {
		t.Fatalf("expected %s, got %s", val.Hex(), got.Hex())
	}

	changes := db.GetStateChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 state change, got %d", len(changes))
	}
	if changes[0].NewValue != val.Hex() {
		t.Fatalf("change new value mismatch: %s", changes[0].NewValue)
	}
}

func TestMemStateDB_SnapshotRevert(t *testing.T) {
	task := &taskPackage{}
	db := NewMemStateDB(task)
	addr := common.HexToAddress("0x3333333333333333333333333333333333333333")

	db.AddBalance(addr, uint256.NewInt(1000), tracing.BalanceChangeUnspecified)
	snap := db.Snapshot()

	db.AddBalance(addr, uint256.NewInt(500), tracing.BalanceChangeUnspecified)
	bal := db.GetBalance(addr)
	if bal.Cmp(uint256.NewInt(1500)) != 0 {
		t.Fatalf("expected 1500, got %s", bal)
	}

	db.RevertToSnapshot(snap)
	bal = db.GetBalance(addr)
	if bal.Cmp(uint256.NewInt(1000)) != 0 {
		t.Fatalf("after revert: expected 1000, got %s", bal)
	}
}

func TestMemStateDB_LoadFromTask(t *testing.T) {
	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0xaaaa",
			Balance: "5000",
			Nonce:   10,
		},
		Receiver: &accountSnapshot{
			Address:  "0xbbbb",
			Balance:  "3000",
			Nonce:    2,
			CodeHash: []byte{0x01, 0x02},
		},
		ContractCode: map[string][]byte{
			"0xbbbb": {0x60, 0x80, 0x60, 0x40},
		},
		StorageSlots: map[string]map[string]string{
			"0xbbbb": {
				"0x0000000000000000000000000000000000000000000000000000000000000000": "0x000000000000000000000000000000000000000000000000000000000000000a",
			},
		},
	}

	db := NewMemStateDB(task)

	senderAddr := common.HexToAddress("0xaaaa")
	if db.GetNonce(senderAddr) != 10 {
		t.Fatalf("sender nonce: expected 10, got %d", db.GetNonce(senderAddr))
	}

	recvAddr := common.HexToAddress("0xbbbb")
	code := db.GetCode(recvAddr)
	if len(code) != 4 {
		t.Fatalf("expected 4 bytes of code, got %d", len(code))
	}

	slot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")
	val := db.GetState(recvAddr, slot)
	expected := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000000a")
	if val != expected {
		t.Fatalf("storage slot mismatch: got %s, want %s", val.Hex(), expected.Hex())
	}
}

func TestMemStateDB_TransientStorageSnapshotRevert(t *testing.T) {
	task := &taskPackage{}
	db := NewMemStateDB(task)
	addr := common.HexToAddress("0x5555555555555555555555555555555555555555")
	slot := common.HexToHash("0x01")
	val := common.HexToHash("0xAB")

	db.SetTransientState(addr, slot, val)
	if got := db.GetTransientState(addr, slot); got != val {
		t.Fatalf("expected %s, got %s", val.Hex(), got.Hex())
	}

	snap := db.Snapshot()

	// Modify transient storage after snapshot
	val2 := common.HexToHash("0xCD")
	db.SetTransientState(addr, slot, val2)
	if got := db.GetTransientState(addr, slot); got != val2 {
		t.Fatalf("expected %s after update, got %s", val2.Hex(), got.Hex())
	}

	// Revert should restore original transient value
	db.RevertToSnapshot(snap)
	if got := db.GetTransientState(addr, slot); got != val {
		t.Fatalf("after revert: expected %s, got %s", val.Hex(), got.Hex())
	}
}

func TestMemStateDB_SelfDestruct(t *testing.T) {
	task := &taskPackage{}
	db := NewMemStateDB(task)
	addr := common.HexToAddress("0x4444444444444444444444444444444444444444")

	db.AddBalance(addr, uint256.NewInt(1000), tracing.BalanceChangeUnspecified)
	if db.HasSelfDestructed(addr) {
		t.Fatal("should not be self-destructed initially")
	}

	db.SelfDestruct(addr)
	if !db.HasSelfDestructed(addr) {
		t.Fatal("should be self-destructed")
	}
	if !db.GetBalance(addr).IsZero() {
		t.Fatal("balance should be zero after self-destruct")
	}
}
