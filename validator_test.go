package main

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"testing"
)

// testCurveN is secp256k1 curve order for generating valid mock signatures.
// Must match secp256k1N in validator.go.
var testCurveN = secp256k1N

// buildValidTxRaw creates a TxRaw with valid secp256k1 signature components.
// Nonce is encoded in the first 8 bytes of payload.
func buildValidTxRaw(nonce uint64) []byte {
	payload := make([]byte, 80)
	binary.BigEndian.PutUint64(payload[:8], nonce)
	// fill rest with random
	rand.Read(payload[8:])

	r, _ := rand.Int(rand.Reader, testCurveN)
	s, _ := rand.Int(rand.Reader, testCurveN)
	// Ensure non-zero (extremely unlikely but be safe)
	for r.Sign() == 0 {
		r, _ = rand.Int(rand.Reader, testCurveN)
	}
	for s.Sign() == 0 {
		s, _ = rand.Int(rand.Reader, testCurveN)
	}

	raw := append(payload, r.FillBytes(make([]byte, 32))...)
	raw = append(raw, s.FillBytes(make([]byte, 32))...)
	raw = append(raw, byte(0)) // v
	return raw
}

func TestValidateL1Valid(t *testing.T) {
	task := &taskPackage{
		TaskID: 1,
		TxRaw:  buildValidTxRaw(1),
	}
	res := validateL1(task)
	if !res.Valid {
		t.Fatalf("expected valid, got: %s", res.RejectReason)
	}
	if res.GasEstimate != 21000 {
		t.Fatalf("expected gas 21000, got %d", res.GasEstimate)
	}
}

func TestValidateL1TooShort(t *testing.T) {
	task := &taskPackage{
		TaskID: 2,
		TxRaw:  make([]byte, 30),
	}
	res := validateL1(task)
	if res.Valid {
		t.Fatal("expected invalid for short tx")
	}
	if res.RejectReason != "tx too short for signature (need >= 65 bytes)" {
		t.Fatalf("unexpected reason: %s", res.RejectReason)
	}
}

func TestValidateL1ZeroR(t *testing.T) {
	// Build a tx with r = 0
	payload := make([]byte, 80)
	rand.Read(payload)
	rBytes := make([]byte, 32) // all zeros
	sBytes := make([]byte, 32)
	s, _ := rand.Int(rand.Reader, testCurveN)
	s.FillBytes(sBytes)

	raw := append(payload, rBytes...)
	raw = append(raw, sBytes...)
	raw = append(raw, byte(0))

	task := &taskPackage{TaskID: 3, TxRaw: raw}
	res := validateL1(task)
	if res.Valid {
		t.Fatal("expected invalid for zero r")
	}
	if res.RejectReason != "signature r is zero" {
		t.Fatalf("unexpected reason: %s", res.RejectReason)
	}
}

func TestValidateL1ZeroS(t *testing.T) {
	payload := make([]byte, 80)
	rand.Read(payload)
	rBytes := make([]byte, 32)
	r, _ := rand.Int(rand.Reader, testCurveN)
	r.FillBytes(rBytes)
	sBytes := make([]byte, 32) // all zeros

	raw := append(payload, rBytes...)
	raw = append(raw, sBytes...)
	raw = append(raw, byte(0))

	task := &taskPackage{TaskID: 4, TxRaw: raw}
	res := validateL1(task)
	if res.Valid {
		t.Fatal("expected invalid for zero s")
	}
	if res.RejectReason != "signature s is zero" {
		t.Fatalf("unexpected reason: %s", res.RejectReason)
	}
}

func TestValidateL1RAboveCurveOrder(t *testing.T) {
	payload := make([]byte, 80)
	rand.Read(payload)
	// r = N (at curve order, should fail)
	rBytes := testCurveN.FillBytes(make([]byte, 32))
	s, _ := rand.Int(rand.Reader, testCurveN)
	sBytes := s.FillBytes(make([]byte, 32))

	raw := append(payload, rBytes...)
	raw = append(raw, sBytes...)
	raw = append(raw, byte(0))

	task := &taskPackage{TaskID: 5, TxRaw: raw}
	res := validateL1(task)
	if res.Valid {
		t.Fatal("expected invalid for r >= curve order")
	}
}

func TestValidateL2Valid(t *testing.T) {
	task := &taskPackage{
		TaskID: 10,
		TxRaw:  buildValidTxRaw(5),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   5, // tx nonce 5 == account nonce 5, valid (next expected)
		},
		Receiver: &accountSnapshot{
			Address: "io1bob",
			Balance: "50000000000000000000",
		},
	}
	res := validateL2(task)
	if !res.Valid {
		t.Fatalf("expected valid, got: %s", res.RejectReason)
	}
	if res.GasEstimate != 21000 {
		t.Fatalf("expected gas 21000, got %d", res.GasEstimate)
	}
}

func TestValidateL2ContractCall(t *testing.T) {
	task := &taskPackage{
		TaskID: 11,
		TxRaw:  buildValidTxRaw(5),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   4,
		},
		Receiver: &accountSnapshot{
			Address:  "io1contract",
			Balance:  "0",
			CodeHash: []byte{0x01, 0x02, 0x03},
		},
	}
	res := validateL2(task)
	if !res.Valid {
		t.Fatalf("expected valid, got: %s", res.RejectReason)
	}
	if res.GasEstimate != 100000 {
		t.Fatalf("expected gas 100000 for contract, got %d", res.GasEstimate)
	}
}

func TestValidateL2ZeroBalance(t *testing.T) {
	task := &taskPackage{
		TaskID: 12,
		TxRaw:  buildValidTxRaw(1),
		Sender: &accountSnapshot{
			Address: "io1broke",
			Balance: "0",
			Nonce:   0,
		},
	}
	res := validateL2(task)
	if res.Valid {
		t.Fatal("expected invalid for zero balance")
	}
	if res.RejectReason != "sender has zero balance" {
		t.Fatalf("unexpected reason: %s", res.RejectReason)
	}
}

func TestValidateL2NonceTooLow(t *testing.T) {
	task := &taskPackage{
		TaskID: 13,
		TxRaw:  buildValidTxRaw(3),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   5, // tx nonce 3 < account nonce 5 → replay
		},
	}
	res := validateL2(task)
	if res.Valid {
		t.Fatal("expected invalid for low nonce")
	}
	if res.RejectReason != "nonce too low: tx=3 account=5" {
		t.Fatalf("unexpected reason: %s", res.RejectReason)
	}
}

func TestValidateL2NonceEqual(t *testing.T) {
	// tx nonce == account nonce should be valid (this IS the expected next nonce)
	task := &taskPackage{
		TaskID: 14,
		TxRaw:  buildValidTxRaw(5),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   5,
		},
	}
	res := validateL2(task)
	if !res.Valid {
		t.Fatalf("expected valid for equal nonce (next expected), got: %s", res.RejectReason)
	}
}

func TestValidateL2MissingSender(t *testing.T) {
	task := &taskPackage{
		TaskID: 15,
		TxRaw:  buildValidTxRaw(1),
		Sender: nil,
	}
	res := validateL2(task)
	if res.Valid {
		t.Fatal("expected invalid for missing sender")
	}
}

func TestValidateL2NilReceiver(t *testing.T) {
	// nil receiver = contract deploy, should be OK
	task := &taskPackage{
		TaskID: 16,
		TxRaw:  buildValidTxRaw(5),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   4,
		},
		Receiver: nil,
	}
	res := validateL2(task)
	if !res.Valid {
		t.Fatalf("expected valid for contract deploy, got: %s", res.RejectReason)
	}
	if res.GasEstimate != 21000 {
		t.Fatalf("expected gas 21000 for nil receiver, got %d", res.GasEstimate)
	}
}

func TestValidateL3Stub(t *testing.T) {
	task := &taskPackage{
		TaskID: 20,
		TxRaw:  buildValidTxRaw(5),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   4,
		},
		Receiver: &accountSnapshot{
			Address: "io1bob",
			Balance: "50000000000000000000",
		},
	}
	res := validateL3(task)
	if !res.Valid {
		t.Fatalf("expected valid, got: %s", res.RejectReason)
	}
	if res.Note != "no EVM tx data, L2 result only" {
		t.Fatalf("unexpected note: %s", res.Note)
	}
}

func TestExtractTxNonce(t *testing.T) {
	raw := make([]byte, 100)
	binary.BigEndian.PutUint64(raw[:8], 42)
	if got := extractTxNonce(raw); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}

	// Too short
	if got := extractTxNonce(make([]byte, 5)); got != 0 {
		t.Fatalf("expected 0 for short input, got %d", got)
	}
}

func TestValidateTaskIntegration(t *testing.T) {
	task := &taskPackage{
		TaskID: 30,
		TxRaw:  buildValidTxRaw(10),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   9,
		},
	}

	// L1
	r := validateTask(task, "L1")
	if !r.Valid {
		t.Fatalf("L1 failed: %s", r.RejectReason)
	}

	// L2
	r = validateTask(task, "L2")
	if !r.Valid {
		t.Fatalf("L2 failed: %s", r.RejectReason)
	}

	// L3
	r = validateTask(task, "L3")
	if !r.Valid {
		t.Fatalf("L3 failed: %s", r.RejectReason)
	}
	// L3 with no EvmTx falls back to L2: Note should be set, RejectReason should be empty
	if r.RejectReason != "" {
		t.Fatalf("expected no reject reason for valid L3 fallback, got: %s", r.RejectReason)
	}
	if r.Note != "no EVM tx data, L2 result only" {
		t.Fatalf("expected L3 fallback note, got: %q", r.Note)
	}
}

func TestValidateL2InvalidBalance(t *testing.T) {
	task := &taskPackage{
		TaskID: 31,
		TxRaw:  buildValidTxRaw(1),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "not-a-number",
			Nonce:   0,
		},
	}
	res := validateL2(task)
	if res.Valid {
		t.Fatal("expected invalid for bad balance string")
	}
}

// Benchmark
func BenchmarkValidateL2(b *testing.B) {
	task := &taskPackage{
		TaskID: 99,
		TxRaw:  buildValidTxRaw(100),
		Sender: &accountSnapshot{
			Address: "io1alice",
			Balance: "100000000000000000000",
			Nonce:   99,
		},
		Receiver: &accountSnapshot{
			Address: "io1bob",
			Balance: "50000000000000000000",
		},
	}
	_ = new(big.Int) // warm up
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		validateL2(task)
	}
}
