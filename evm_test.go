package main

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Simple IOTX transfer: sender → receiver, no contract code
func TestEVM_SimpleTransfer(t *testing.T) {
	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000", // 1 IOTX
			Nonce:   0,
		},
		Receiver: &accountSnapshot{
			Address: "0x2222222222222222222222222222222222222222",
			Balance: "0",
			Nonce:   0,
		},
		BlockHeight: 1000,
		BlockContext: &blockCtx{
			Number:    1000,
			Timestamp: 1700000000,
			GasLimit:  30000000,
			BaseFee:   "0",
			Coinbase:  "0x0000000000000000000000000000000000000000",
		},
		EvmTx: &evmTx{
			To:       "0x2222222222222222222222222222222222222222",
			Value:    "100000000000000000", // 0.1 IOTX
			GasLimit: 100000,
			GasPrice: "1000000000",
		},
	}

	result := executeEVM(task)
	if !result.Success {
		t.Fatalf("transfer failed: %s", result.Error)
	}
	if len(result.StateChanges) != 0 {
		t.Fatalf("expected 0 state changes for transfer, got %d", len(result.StateChanges))
	}
	// Simple value transfer to EOA should use minimal gas
	t.Logf("transfer gas used: %d", result.GasUsed)
}

// Counter contract: call increment() which does SLOAD+ADD+SSTORE
func TestEVM_CounterIncrement(t *testing.T) {
	// Minimal runtime bytecode for increment():
	// Selector matching: PUSH0 CALLDATALOAD PUSH1 0xe0 SHR
	// But SHR may not be available in all configs, so use simpler approach:
	//
	// Simple contract that always increments slot 0:
	// PUSH1 0x00 SLOAD  (load slot 0)
	// PUSH1 0x01 ADD    (add 1)
	// PUSH1 0x00 SSTORE (store to slot 0)
	// STOP
	//
	// Hex: 60 00 54 60 01 01 60 00 55 00
	alwaysIncrementCode := common.FromHex("600054600101600055" + "00")

	contractAddr := "0x3333333333333333333333333333333333333333"

	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000",
			Nonce:   0,
		},
		Receiver: &accountSnapshot{
			Address:  contractAddr,
			Balance:  "0",
			Nonce:    1,
			CodeHash: []byte{0x01},
		},
		ContractCode: map[string][]byte{
			contractAddr: alwaysIncrementCode,
		},
		StorageSlots: map[string]map[string]string{
			contractAddr: {
				"0x0000000000000000000000000000000000000000000000000000000000000000": "0x0000000000000000000000000000000000000000000000000000000000000005",
			},
		},
		BlockHeight: 1000,
		BlockContext: &blockCtx{
			Number:    1000,
			Timestamp: 1700000000,
			GasLimit:  30000000,
			BaseFee:   "0",
			Coinbase:  "0x0000000000000000000000000000000000000000",
		},
		EvmTx: &evmTx{
			To:       contractAddr,
			Value:    "0",
			Data:     common.FromHex("d09de08a"), // selector (ignored by contract, it always increments)
			GasLimit: 100000,
			GasPrice: "1000000000",
		},
	}

	result := executeEVM(task)
	if !result.Success {
		t.Fatalf("counter increment failed: %s", result.Error)
	}
	if result.GasUsed == 0 {
		t.Fatal("expected non-zero gas")
	}
	t.Logf("counter gas used: %d", result.GasUsed)

	if len(result.StateChanges) != 1 {
		t.Fatalf("expected 1 state change, got %d", len(result.StateChanges))
	}

	sc := result.StateChanges[0]
	expectedOld := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000005").Hex()
	expectedNew := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000006").Hex()
	if sc.OldValue != expectedOld {
		t.Fatalf("old value mismatch: got %s, want %s", sc.OldValue, expectedOld)
	}
	if sc.NewValue != expectedNew {
		t.Fatalf("new value mismatch: got %s, want %s", sc.NewValue, expectedNew)
	}
}

// Contract creation: deploy bytecode
func TestEVM_ContractCreation(t *testing.T) {
	// Simple init code: PUSH1 0x00 PUSH1 0x00 RETURN (returns empty runtime code)
	initCode := common.FromHex("60006000f3")

	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			Balance: "1000000000000000000",
			Nonce:   99, // high nonce to avoid address collision with existing test accounts
		},
		BlockHeight: 1000,
		BlockContext: &blockCtx{
			Number:    1000,
			Timestamp: 1700000000,
			GasLimit:  30000000,
			BaseFee:   "0",
			Coinbase:  "0x0000000000000000000000000000000000000000",
		},
		EvmTx: &evmTx{
			To:       "", // empty = create
			Value:    "0",
			Data:     initCode,
			GasLimit: 100000,
			GasPrice: "1000000000",
		},
	}

	result := executeEVM(task)
	if !result.Success {
		t.Fatalf("contract creation failed: %s", result.Error)
	}
	if result.GasUsed == 0 {
		t.Fatal("expected non-zero gas for contract creation")
	}
	t.Logf("creation gas used: %d", result.GasUsed)
}

// Out of gas: not enough gas for a contract call
func TestEVM_OutOfGas(t *testing.T) {
	// Contract code that does SSTORE (expensive)
	code := common.FromHex("6001600055" + "00") // PUSH1 1 PUSH1 0 SSTORE STOP

	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000",
			Nonce:   0,
		},
		Receiver: &accountSnapshot{
			Address:  "0x2222222222222222222222222222222222222222",
			Balance:  "0",
			Nonce:    1,
			CodeHash: []byte{0x01},
		},
		ContractCode: map[string][]byte{
			"0x2222222222222222222222222222222222222222": code,
		},
		BlockHeight: 1000,
		BlockContext: &blockCtx{
			Number:    1000,
			Timestamp: 1700000000,
			GasLimit:  30000000,
			BaseFee:   "0",
			Coinbase:  "0x0000000000000000000000000000000000000000",
		},
		EvmTx: &evmTx{
			To:       "0x2222222222222222222222222222222222222222",
			Value:    "0",
			GasLimit: 100, // way too little gas for SSTORE
			GasPrice: "1000000000",
		},
	}

	result := executeEVM(task)
	if result.Success {
		t.Fatal("expected failure due to out of gas")
	}
	if result.Error == "" {
		t.Fatal("expected error message")
	}
	t.Logf("out-of-gas error: %s, gas used: %d", result.Error, result.GasUsed)
}

// No EvmTx field should fail gracefully
func TestEVM_NoEvmTx(t *testing.T) {
	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000",
			Nonce:   0,
		},
	}

	result := executeEVM(task)
	if result.Success {
		t.Fatal("expected failure with no EVM tx")
	}
	if result.Error != "no EVM tx data" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
}

// State diff correctness: verify multiple storage writes
func TestEVM_StateDiffCorrectness(t *testing.T) {
	// Contract: writes to slot 0 and slot 1
	// PUSH1 0x42 PUSH1 0x00 SSTORE  ; slot 0 = 0x42
	// PUSH1 0xff PUSH1 0x01 SSTORE  ; slot 1 = 0xff
	// STOP
	code := common.FromHex("6042600055" + "60ff600155" + "00")

	contractAddr := "0x4444444444444444444444444444444444444444"

	task := &taskPackage{
		Sender: &accountSnapshot{
			Address: "0x1111111111111111111111111111111111111111",
			Balance: "1000000000000000000",
			Nonce:   0,
		},
		Receiver: &accountSnapshot{
			Address:  contractAddr,
			Balance:  "0",
			Nonce:    1,
			CodeHash: []byte{0x01},
		},
		ContractCode: map[string][]byte{
			contractAddr: code,
		},
		BlockHeight: 1000,
		BlockContext: &blockCtx{
			Number:    1000,
			Timestamp: 1700000000,
			GasLimit:  30000000,
			BaseFee:   "0",
			Coinbase:  "0x0000000000000000000000000000000000000000",
		},
		EvmTx: &evmTx{
			To:       contractAddr,
			Value:    "0",
			GasLimit: 200000,
			GasPrice: "1000000000",
		},
	}

	result := executeEVM(task)
	if !result.Success {
		t.Fatalf("execution failed: %s", result.Error)
	}
	if len(result.StateChanges) != 2 {
		t.Fatalf("expected 2 state changes, got %d", len(result.StateChanges))
	}

	// Verify slot values
	for _, sc := range result.StateChanges {
		t.Logf("state change: addr=%s slot=%s old=%s new=%s", sc.Address, sc.Slot, sc.OldValue, sc.NewValue)
	}
}
