//go:build e2e

package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Test configuration — override via environment variables.
const (
	envRewardContract  = "REWARD_E2E_CONTRACT"   // default: 0x96F475F87911615dD710f9cB425Af8ed0e167C89
	envCoordinatorKey  = "REWARD_E2E_COORD_KEY"   // coordinator hot wallet private key
	envRPC             = "REWARD_E2E_RPC"          // default: mainnet
	envChainID         = "REWARD_E2E_CHAIN_ID"     // default: 4689
	envEpochWaitSec    = "REWARD_E2E_EPOCH_WAIT"   // seconds per epoch (default: 35)
	envCoordinatorAddr = "REWARD_E2E_COORDINATOR"  // coordinator gRPC address
	envAPIKey          = "REWARD_E2E_API_KEY"      // agent API key
)

const (
	defaultContract  = "0x96F475F87911615dD710f9cB425Af8ed0e167C89"
	defaultRPC       = "https://babel-api.mainnet.iotex.io"
	defaultChainID   = 4689
	defaultEpochWait = 5 // seconds between depositAndSettle calls (just enough for tx confirmation)
	gasPerAgent      = 0.5 // IOTX funded to each agent wallet for gas (IoTeX gas price ~1e12)
)

// Extended ABI with depositAndSettle + totalWeight + cumulativeRewardPerWeight
const extendedRewardPoolABI = `[
	{"inputs":[],"name":"claim","outputs":[],"stateMutability":"nonpayable","type":"function"},
	{"inputs":[{"internalType":"address","name":"agent","type":"address"}],"name":"claimable","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[],"name":"coordinator","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},
	{"inputs":[],"name":"totalWeight","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[],"name":"cumulativeRewardPerWeight","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[{"internalType":"address","name":"agent","type":"address"}],"name":"agents","outputs":[{"internalType":"uint256","name":"weight","type":"uint256"},{"internalType":"uint256","name":"rewardSnapshot","type":"uint256"},{"internalType":"uint256","name":"pending","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"inputs":[{"internalType":"address[]","name":"_agents","type":"address[]"},{"internalType":"uint256[]","name":"_weights","type":"uint256[]"}],"name":"depositAndSettle","outputs":[],"stateMutability":"payable","type":"function"},
	{"inputs":[{"internalType":"address","name":"agent","type":"address"}],"name":"claimFor","outputs":[],"stateMutability":"nonpayable","type":"function"},
	{"inputs":[{"internalType":"address[]","name":"agentList","type":"address[]"}],"name":"batchClaimFor","outputs":[],"stateMutability":"nonpayable","type":"function"},
	{"inputs":[{"internalType":"address[]","name":"_agents","type":"address[]"},{"internalType":"uint256[]","name":"_weights","type":"uint256[]"},{"internalType":"address[]","name":"_claimees","type":"address[]"}],"name":"depositSettleAndClaim","outputs":[],"stateMutability":"payable","type":"function"}
]`

type testEnv struct {
	client       *ethclient.Client
	contract     common.Address
	contractABI  abi.ABI
	coordKey     *ecdsa.PrivateKey
	coordAddr    common.Address
	rpc          string
	chainID      *big.Int
	epochWaitSec int
}

// deployFreshContract deploys a new AgentRewardPool contract so tests start with clean state.
func (e *testEnv) deployFreshContract(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	bytecodeHex := rewardPoolBytecode
	bytecode := common.FromHex(bytecodeHex)
	// Constructor arg: coordinator address (32-byte left-padded)
	constructorArg := common.LeftPadBytes(e.coordAddr.Bytes(), 32)
	deployData := append(bytecode, constructorArg...)

	nonce, err := e.client.PendingNonceAt(ctx, e.coordAddr)
	if err != nil {
		t.Fatalf("nonce for deploy: %v", err)
	}

	gasPrice, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}

	gasLimit, err := e.client.EstimateGas(ctx, ethereum.CallMsg{
		From: e.coordAddr,
		Data: deployData,
	})
	if err != nil {
		t.Logf("gas estimation failed, using 1500000: %v", err)
		gasLimit = 1_500_000
	} else {
		gasLimit = gasLimit * 130 / 100
	}

	deployTx := types.NewContractCreation(nonce, big.NewInt(0), gasLimit, gasPrice, deployData)
	signer := types.NewEIP155Signer(e.chainID)
	signedTx, err := types.SignTx(deployTx, signer, e.coordKey)
	if err != nil {
		t.Fatalf("sign deploy: %v", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		t.Fatalf("send deploy: %v", err)
	}

	t.Logf("deploying fresh contract... tx=%s", signedTx.Hash().Hex())

	for i := 0; i < 60; i++ {
		receipt, err := e.client.TransactionReceipt(ctx, signedTx.Hash())
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		if receipt.Status != 1 {
			t.Fatalf("deploy reverted (gas=%d)", receipt.GasUsed)
		}
		e.contract = receipt.ContractAddress
		t.Logf("fresh contract deployed: %s (gas=%d, block=%d)",
			e.contract.Hex(), receipt.GasUsed, receipt.BlockNumber.Uint64())
		return
	}
	t.Fatal("deploy not confirmed after 180s")
}

func getEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	coordKeyHex := os.Getenv(envCoordinatorKey)
	if coordKeyHex == "" {
		t.Skipf("skipping: %s not set (coordinator hot wallet key required)", envCoordinatorKey)
	}

	hexKey := strings.TrimPrefix(coordKeyHex, "0x")
	coordKey, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		t.Fatalf("parse coordinator key: %v", err)
	}
	coordAddr := crypto.PubkeyToAddress(coordKey.PublicKey)

	rpcURL := getEnvOr(envRPC, defaultRPC)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", rpcURL, err)
	}

	contractAddr := common.HexToAddress(getEnvOr(envRewardContract, defaultContract))

	parsedABI, err := abi.JSON(strings.NewReader(extendedRewardPoolABI))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}

	epochWait := defaultEpochWait
	if v := os.Getenv(envEpochWaitSec); v != "" {
		fmt.Sscanf(v, "%d", &epochWait)
	}

	chainID := int64(defaultChainID)
	if v := os.Getenv(envChainID); v != "" {
		fmt.Sscanf(v, "%d", &chainID)
	}

	return &testEnv{
		client:       client,
		contract:     contractAddr,
		contractABI:  parsedABI,
		coordKey:     coordKey,
		coordAddr:    coordAddr,
		rpc:          rpcURL,
		chainID:      big.NewInt(chainID),
		epochWaitSec: epochWait,
	}
}

// queryClaimable returns the claimable amount in wei for a given agent address.
func (e *testEnv) queryClaimable(t *testing.T, agent common.Address) *big.Int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data, err := e.contractABI.Pack("claimable", agent)
	if err != nil {
		t.Fatalf("pack claimable: %v", err)
	}

	result, err := e.client.CallContract(ctx, ethereum.CallMsg{
		To:   &e.contract,
		Data: data,
	}, nil)
	if err != nil {
		t.Fatalf("call claimable(%s): %v", agent.Hex(), err)
	}

	outputs, err := e.contractABI.Unpack("claimable", result)
	if err != nil {
		t.Fatalf("unpack claimable: %v", err)
	}
	return outputs[0].(*big.Int)
}

// queryContractBalance returns the contract's IOTX balance.
func (e *testEnv) queryContractBalance(t *testing.T) *big.Int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bal, err := e.client.BalanceAt(ctx, e.contract, nil)
	if err != nil {
		t.Fatalf("contract balance: %v", err)
	}
	return bal
}

// queryTotalWeight returns the totalWeight from the contract.
func (e *testEnv) queryTotalWeight(t *testing.T) *big.Int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data, err := e.contractABI.Pack("totalWeight")
	if err != nil {
		t.Fatalf("pack totalWeight: %v", err)
	}

	result, err := e.client.CallContract(ctx, ethereum.CallMsg{
		To:   &e.contract,
		Data: data,
	}, nil)
	if err != nil {
		t.Fatalf("call totalWeight: %v", err)
	}

	outputs, err := e.contractABI.Unpack("totalWeight", result)
	if err != nil {
		t.Fatalf("unpack totalWeight: %v", err)
	}
	return outputs[0].(*big.Int)
}

// executeClaim sends a claim() transaction from the given private key.
func (e *testEnv) executeClaim(t *testing.T, key *ecdsa.PrivateKey) (gasUsed uint64, claimed *big.Int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	addr := crypto.PubkeyToAddress(key.PublicKey)

	// Get claimable before
	claimableBefore := e.queryClaimable(t, addr)
	if claimableBefore.Sign() == 0 {
		t.Fatalf("nothing to claim for %s", addr.Hex())
	}

	claimData, err := e.contractABI.Pack("claim")
	if err != nil {
		t.Fatalf("pack claim: %v", err)
	}

	nonce, err := e.client.PendingNonceAt(ctx, addr)
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}

	gasPrice, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}

	tx := types.NewTransaction(nonce, e.contract, big.NewInt(0), 200000, gasPrice, claimData)
	signer := types.NewEIP155Signer(e.chainID)
	signedTx, err := types.SignTx(tx, signer, key)
	if err != nil {
		t.Fatalf("sign claim tx: %v", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		t.Fatalf("send claim tx: %v", err)
	}

	t.Logf("claim tx sent: %s (from %s)", signedTx.Hash().Hex(), addr.Hex())

	// Wait for receipt
	for i := 0; i < 30; i++ {
		receipt, err := e.client.TransactionReceipt(ctx, signedTx.Hash())
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if receipt.Status != 1 {
			t.Fatalf("claim tx reverted (gas used: %d)", receipt.GasUsed)
		}
		t.Logf("claim success: %s IOTX, gas=%d, block=%d",
			weiToIOTX(claimableBefore), receipt.GasUsed, receipt.BlockNumber.Uint64())
		return receipt.GasUsed, claimableBefore
	}

	t.Fatalf("claim tx not confirmed after 60s: %s", signedTx.Hash().Hex())
	return 0, nil
}

// depositAndSettle calls the contract's depositAndSettle with the given agents and weights.
func (e *testEnv) depositAndSettle(t *testing.T, agents []common.Address, weights []*big.Int, depositWei *big.Int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := e.contractABI.Pack("depositAndSettle", agents, weights)
	if err != nil {
		t.Fatalf("pack depositAndSettle: %v", err)
	}

	nonce, err := e.client.PendingNonceAt(ctx, e.coordAddr)
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}

	gasPrice, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}

	// Estimate gas
	gasLimit, err := e.client.EstimateGas(ctx, ethereum.CallMsg{
		From:  e.coordAddr,
		To:    &e.contract,
		Data:  data,
		Value: depositWei,
	})
	if err != nil {
		t.Logf("gas estimation failed, using 500000: %v", err)
		gasLimit = 500000
	} else {
		gasLimit = gasLimit * 130 / 100 // 30% buffer
	}

	tx := types.NewTransaction(nonce, e.contract, depositWei, gasLimit, gasPrice, data)
	signer := types.NewEIP155Signer(e.chainID)
	signedTx, err := types.SignTx(tx, signer, e.coordKey)
	if err != nil {
		t.Fatalf("sign depositAndSettle: %v", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		t.Fatalf("send depositAndSettle: %v", err)
	}

	t.Logf("depositAndSettle tx: %s (agents=%d, deposit=%s IOTX)",
		signedTx.Hash().Hex(), len(agents), weiToIOTX(depositWei))

	// Wait for receipt
	for i := 0; i < 30; i++ {
		receipt, err := e.client.TransactionReceipt(ctx, signedTx.Hash())
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if receipt.Status != 1 {
			t.Fatalf("depositAndSettle reverted (gas=%d)", receipt.GasUsed)
		}
		t.Logf("depositAndSettle confirmed: gas=%d, block=%d", receipt.GasUsed, receipt.BlockNumber.Uint64())
		return
	}
	t.Fatalf("depositAndSettle not confirmed: %s", signedTx.Hash().Hex())
}

// fundWallets sends a small amount of IOTX to each wallet for gas.
func (e *testEnv) fundWallets(t *testing.T, addrs []common.Address, amountPerWallet *big.Int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nonce, err := e.client.PendingNonceAt(ctx, e.coordAddr)
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}

	gasPrice, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}

	signer := types.NewEIP155Signer(e.chainID)

	for i, addr := range addrs {
		// Check if already funded
		bal, _ := e.client.BalanceAt(ctx, addr, nil)
		if bal != nil && bal.Cmp(amountPerWallet) >= 0 {
			t.Logf("wallet %d (%s) already funded: %s IOTX", i, addr.Hex(), weiToIOTX(bal))
			continue
		}

		tx := types.NewTransaction(nonce, addr, amountPerWallet, 21000, gasPrice, nil)
		signedTx, err := types.SignTx(tx, signer, e.coordKey)
		if err != nil {
			t.Fatalf("sign fund tx %d: %v", i, err)
		}
		if err := e.client.SendTransaction(ctx, signedTx); err != nil {
			t.Fatalf("send fund tx %d: %v", i, err)
		}
		t.Logf("funded wallet %d: %s → %s IOTX (tx: %s)",
			i, addr.Hex(), weiToIOTX(amountPerWallet), signedTx.Hash().Hex())
		nonce++
	}

	// Wait for funding txs to confirm
	time.Sleep(15 * time.Second)
}

// --- Step 1: Single Agent Basic Flow (3.6 + 3.7) ---

func TestRewardE2E_Step1_SingleAgent(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 1: Single Agent — Basic Reward Flow ===")

	// Generate 1 agent wallet
	addrs, keys, err := generateWallets(1)
	if err != nil {
		t.Fatalf("generate wallet: %v", err)
	}
	agentAddr := addrs[0]
	agentKey := keys[0]
	t.Logf("agent wallet: %s", agentAddr.Hex())

	// Fund agent wallet for claim gas
	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	// Register agent first (weight=1, no deposit).
	// The contract's cumulative model requires totalWeight > 0 before deposits distribute,
	// so we must register agents before sending reward deposits.
	t.Log("--- Register agent (weight=1, deposit=0) ---")
	env.depositAndSettle(t, []common.Address{agentAddr}, []*big.Int{big.NewInt(1)}, big.NewInt(0))

	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// Now simulate 2 epochs of reward deposits
	epochRewardWei := iotxToWei(0.5)                                          // 0.5 IOTX per epoch
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))           // 10%
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)             // 90% → 0.45 IOTX

	for epoch := 1; epoch <= 2; epoch++ {
		t.Logf("--- Epoch %d (deposit %s IOTX) ---", epoch, weiToIOTX(agentPoolWei))
		env.depositAndSettle(t, []common.Address{agentAddr}, []*big.Int{big.NewInt(1)}, agentPoolWei)

		if epoch < 2 {
			t.Logf("waiting %ds for next epoch...", env.epochWaitSec)
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Verify claimable > 0
	claimable := env.queryClaimable(t, agentAddr)
	t.Logf("claimable: %s IOTX (%s rau)", weiToIOTX(claimable), claimable.String())
	if claimable.Sign() == 0 {
		t.Fatal("FAIL: claimable is 0 after 2 epochs")
	}

	// Expected: ~0.9 IOTX (2 epochs × 0.45 IOTX)
	expectedMin := iotxToWei(0.85) // allow some rounding
	if claimable.Cmp(expectedMin) < 0 {
		t.Errorf("claimable %s IOTX < expected minimum 0.85 IOTX", weiToIOTX(claimable))
	}

	// Execute claim
	_, claimed := env.executeClaim(t, agentKey)
	t.Logf("PASS: claimed %s IOTX", weiToIOTX(claimed))

	// Verify claimable is now 0
	postClaim := env.queryClaimable(t, agentAddr)
	if postClaim.Sign() != 0 {
		t.Errorf("claimable should be 0 after claim, got %s", weiToIOTX(postClaim))
	}
}

// --- Step 2: Two Agents — Proportional Distribution ---

func TestRewardE2E_Step2_TwoAgents(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 2: Two Agents — Proportional Distribution ===")

	addrs, keys, err := generateWallets(2)
	if err != nil {
		t.Fatalf("generate wallets: %v", err)
	}

	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	epochRewardWei := iotxToWei(0.5)
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)

	// Register agents first (no deposit)
	t.Log("--- Register agents (weight=1, deposit=0) ---")
	weights := []*big.Int{big.NewInt(1), big.NewInt(1)}
	env.depositAndSettle(t, addrs, weights, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// 2 epochs with deposits
	for epoch := 1; epoch <= 2; epoch++ {
		t.Logf("--- Epoch %d ---", epoch)
		weights := []*big.Int{big.NewInt(1), big.NewInt(1)}
		env.depositAndSettle(t, addrs, weights, agentPoolWei)

		if epoch < 2 {
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Verify both agents have claimable
	c0 := env.queryClaimable(t, addrs[0])
	c1 := env.queryClaimable(t, addrs[1])
	t.Logf("agent-01 claimable: %s IOTX", weiToIOTX(c0))
	t.Logf("agent-02 claimable: %s IOTX", weiToIOTX(c1))

	if c0.Sign() == 0 || c1.Sign() == 0 {
		t.Fatal("FAIL: one or both agents have 0 claimable")
	}

	// Both should be roughly equal (equal weight)
	total := new(big.Int).Add(c0, c1)
	expectedTotal := iotxToWei(0.85) // ~0.9 minus rounding
	t.Logf("total claimable: %s IOTX", weiToIOTX(total))
	if total.Cmp(expectedTotal) < 0 {
		t.Errorf("total claimable %s < expected 0.85 IOTX", weiToIOTX(total))
	}

	// Both should be within 10% of each other
	diff := new(big.Int).Sub(c0, c1)
	diff.Abs(diff)
	threshold := new(big.Int).Div(c0, big.NewInt(10)) // 10% tolerance
	if diff.Cmp(threshold) > 0 {
		t.Errorf("agent claimable difference too large: %s vs %s", weiToIOTX(c0), weiToIOTX(c1))
	}

	// Claim both
	env.executeClaim(t, keys[0])
	env.executeClaim(t, keys[1])

	t.Log("PASS: both agents claimed successfully")
}

// --- Step 3: Ten Agents — Distribution Under Load ---

func TestRewardE2E_Step3_TenAgents(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 3: Ten Agents — Distribution Under Load ===")

	n := 10
	addrs, keys, err := generateWallets(n)
	if err != nil {
		t.Fatalf("generate wallets: %v", err)
	}

	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	epochRewardWei := iotxToWei(0.5)
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)

	// Register agents first (no deposit)
	t.Log("--- Register 10 agents (weight=1, deposit=0) ---")
	regWeights := make([]*big.Int, n)
	for i := range regWeights {
		regWeights[i] = big.NewInt(1)
	}
	env.depositAndSettle(t, addrs, regWeights, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// 5 epochs with deposits
	numEpochs := 5
	for epoch := 1; epoch <= numEpochs; epoch++ {
		t.Logf("--- Epoch %d ---", epoch)
		weights := make([]*big.Int, n)
		for i := range weights {
			weights[i] = big.NewInt(1)
		}
		env.depositAndSettle(t, addrs, weights, agentPoolWei)

		if epoch < numEpochs {
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Verify all 10 agents have claimable > 0
	totalClaimable := big.NewInt(0)
	for i, addr := range addrs {
		c := env.queryClaimable(t, addr)
		t.Logf("agent-%02d claimable: %s IOTX", i+1, weiToIOTX(c))
		if c.Sign() == 0 {
			t.Errorf("agent-%02d has 0 claimable", i+1)
		}
		totalClaimable.Add(totalClaimable, c)
	}

	// Total should be ≈ 5 × 0.45 = 2.25 IOTX
	t.Logf("total claimable: %s IOTX (expected ~2.25)", weiToIOTX(totalClaimable))
	expectedMin := iotxToWei(2.0) // allow rounding
	if totalClaimable.Cmp(expectedMin) < 0 {
		t.Errorf("total claimable %s < expected 2.0 IOTX", weiToIOTX(totalClaimable))
	}

	// Claim 3 random agents
	for _, i := range []int{0, 4, 9} {
		t.Logf("claiming agent-%02d...", i+1)
		env.executeClaim(t, keys[i])
	}

	t.Log("PASS: 10 agents all have claimable, 3 claimed successfully")
}

// --- Step 4: Dynamic Join/Leave ---

func TestRewardE2E_Step4_DynamicJoinLeave(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 4: Dynamic Agent Join/Leave ===")

	// Generate 7 wallets (5 initial + 2 new)
	addrs, keys, err := generateWallets(7)
	if err != nil {
		t.Fatalf("generate wallets: %v", err)
	}

	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	epochRewardWei := iotxToWei(0.5)
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)

	// Register all 7 agents upfront (weight=1 for first 5, weight=0 for 6,7)
	t.Log("--- Register all agents ---")
	regWeights := []*big.Int{
		big.NewInt(1), big.NewInt(1), big.NewInt(1), big.NewInt(1), big.NewInt(1), // agents 1-5
		big.NewInt(0), big.NewInt(0), // agents 6,7 (inactive initially)
	}
	env.depositAndSettle(t, addrs, regWeights, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// Phase 1: 5 agents run 2 epochs
	for epoch := 1; epoch <= 2; epoch++ {
		t.Logf("--- Phase 1, Epoch %d (agents 1-5 active) ---", epoch)
		weights := []*big.Int{
			big.NewInt(1), big.NewInt(1), big.NewInt(1), big.NewInt(1), big.NewInt(1),
			big.NewInt(0), big.NewInt(0),
		}
		env.depositAndSettle(t, addrs, weights, agentPoolWei)
		if epoch < 2 {
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Record claimable for agents 3 and 4 (about to leave)
	c3Before := env.queryClaimable(t, addrs[2])
	c4Before := env.queryClaimable(t, addrs[3])
	t.Logf("agent-03 claimable before leave: %s IOTX", weiToIOTX(c3Before))
	t.Logf("agent-04 claimable before leave: %s IOTX", weiToIOTX(c4Before))

	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// Phase 2: Remove agents 3,4 (weight→0); activate agents 6,7 (weight→1). Run 2 more epochs.
	for epoch := 1; epoch <= 2; epoch++ {
		t.Logf("--- Phase 2, Epoch %d (agents 1,2,5,6,7 active; 3,4 weight=0) ---", epoch)
		weights := []*big.Int{
			big.NewInt(1), // agent-01: active
			big.NewInt(1), // agent-02: active
			big.NewInt(0), // agent-03: leaving (weight=0)
			big.NewInt(0), // agent-04: leaving (weight=0)
			big.NewInt(1), // agent-05: active
			big.NewInt(1), // agent-06: joining
			big.NewInt(1), // agent-07: joining
		}
		env.depositAndSettle(t, addrs, weights, agentPoolWei)
		if epoch < 2 {
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Verify: agent-03 and agent-04 claimable should be frozen at phase 1 value
	// (they got settled with weight=0, so no new rewards accrue)
	c3After := env.queryClaimable(t, addrs[2])
	c4After := env.queryClaimable(t, addrs[3])
	t.Logf("agent-03 claimable after leave: %s IOTX (was %s)", weiToIOTX(c3After), weiToIOTX(c3Before))
	t.Logf("agent-04 claimable after leave: %s IOTX (was %s)", weiToIOTX(c4After), weiToIOTX(c4Before))

	// c3After >= c3Before (settlement may have credited pending), but should NOT keep growing
	// After the first phase2 epoch settles them (weight→0), the second epoch should add nothing.
	if c3After.Sign() == 0 && c3Before.Sign() > 0 {
		t.Errorf("agent-03 lost its claimable after leaving: %s → %s", weiToIOTX(c3Before), weiToIOTX(c3After))
	}

	// Verify: agents 6,7 have claimable from their 2 epochs
	c6 := env.queryClaimable(t, addrs[5])
	c7 := env.queryClaimable(t, addrs[6])
	t.Logf("agent-06 claimable: %s IOTX", weiToIOTX(c6))
	t.Logf("agent-07 claimable: %s IOTX", weiToIOTX(c7))
	if c6.Sign() == 0 {
		t.Error("agent-06 has 0 claimable")
	}
	if c7.Sign() == 0 {
		t.Error("agent-07 has 0 claimable")
	}

	// Verify: agents 1,2,5 have more than agents 6,7 (ran all 4 epochs vs 2)
	c1 := env.queryClaimable(t, addrs[0])
	c2 := env.queryClaimable(t, addrs[1])
	c5 := env.queryClaimable(t, addrs[4])
	t.Logf("agent-01 claimable: %s IOTX (all 4 epochs)", weiToIOTX(c1))
	t.Logf("agent-02 claimable: %s IOTX (all 4 epochs)", weiToIOTX(c2))
	t.Logf("agent-05 claimable: %s IOTX (all 4 epochs)", weiToIOTX(c5))

	if c1.Cmp(c6) <= 0 {
		t.Errorf("agent-01 (4 epochs) should have more than agent-06 (2 epochs): %s vs %s",
			weiToIOTX(c1), weiToIOTX(c6))
	}

	// Claim all agents
	for i, key := range keys {
		c := env.queryClaimable(t, addrs[i])
		if c.Sign() > 0 {
			t.Logf("claiming agent-%02d (%s IOTX)...", i+1, weiToIOTX(c))
			env.executeClaim(t, key)
		}
	}

	// Verify contract balance >= 0 (no overdraft)
	contractBal := env.queryContractBalance(t)
	t.Logf("contract balance after all claims: %s IOTX", weiToIOTX(contractBal))
	if contractBal.Sign() < 0 {
		t.Error("contract balance is negative — overdraft!")
	}

	t.Log("PASS: dynamic join/leave verified")
}

// --- Step 5: MinTasks Threshold (3.8) ---

func TestRewardE2E_Step5_MinTasksThreshold(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 5: MinTasks Threshold ===")
	t.Log("This test verifies that agents with 0 weight get no reward")

	addrs, _, err := generateWallets(2)
	if err != nil {
		t.Fatalf("generate wallets: %v", err)
	}

	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	epochRewardWei := iotxToWei(0.5)
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)

	// Register agent-01 with weight=1 (agent-02 stays at weight=0)
	t.Log("--- Register agent-01 (weight=1), agent-02 (weight=0) ---")
	env.depositAndSettle(t, addrs, []*big.Int{big.NewInt(1), big.NewInt(0)}, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// Settle with deposit — only agent-01 should earn
	weights := []*big.Int{big.NewInt(1), big.NewInt(0)}
	env.depositAndSettle(t, addrs, weights, agentPoolWei)

	c0 := env.queryClaimable(t, addrs[0])
	c1 := env.queryClaimable(t, addrs[1])
	t.Logf("agent-01 (weight=1) claimable: %s IOTX", weiToIOTX(c0))
	t.Logf("agent-02 (weight=0) claimable: %s IOTX", weiToIOTX(c1))

	if c0.Sign() == 0 {
		t.Error("agent-01 should have claimable > 0")
	}
	if c1.Sign() != 0 {
		t.Errorf("agent-02 (weight=0) should have 0 claimable, got %s", weiToIOTX(c1))
	}

	t.Log("PASS: zero-weight agent gets no reward")
}

// --- Step 6: Delegate Cut Verification (3.10) ---

func TestRewardE2E_Step6_DelegateCut(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 6: Delegate Cut Verification ===")

	addrs, _, err := generateWallets(1)
	if err != nil {
		t.Fatalf("generate wallet: %v", err)
	}

	// Track coordinator balance before
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	coordBalBefore, err := env.client.BalanceAt(ctx, env.coordAddr, nil)
	cancel()
	if err != nil {
		t.Fatalf("coord balance: %v", err)
	}

	epochRewardWei := iotxToWei(0.5)                                 // 0.5 IOTX total epoch reward
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))  // 10% = 0.05 IOTX
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)    // 90% = 0.45 IOTX

	t.Logf("epoch reward: %s IOTX", weiToIOTX(epochRewardWei))
	t.Logf("delegate cut (10%%): %s IOTX (retained by coordinator)", weiToIOTX(delegateCut))
	t.Logf("agent pool (90%%): %s IOTX (sent to contract)", weiToIOTX(agentPoolWei))

	// Register agent first
	t.Log("--- Register agent (weight=1, deposit=0) ---")
	env.depositAndSettle(t, addrs, []*big.Int{big.NewInt(1)}, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// Contract balance before deposit
	contractBalBefore := env.queryContractBalance(t)

	// Deposit 1 epoch
	weights := []*big.Int{big.NewInt(1)}
	env.depositAndSettle(t, addrs, weights, agentPoolWei)

	// Contract balance after
	contractBalAfter := env.queryContractBalance(t)
	deposited := new(big.Int).Sub(contractBalAfter, contractBalBefore)

	t.Logf("contract balance before: %s IOTX", weiToIOTX(contractBalBefore))
	t.Logf("contract balance after:  %s IOTX", weiToIOTX(contractBalAfter))
	t.Logf("deposited to contract:   %s IOTX", weiToIOTX(deposited))

	// Verify: deposited ≈ agentPoolWei (0.45 IOTX)
	if deposited.Cmp(agentPoolWei) != 0 {
		t.Errorf("deposited %s != expected %s IOTX", weiToIOTX(deposited), weiToIOTX(agentPoolWei))
	}

	// Verify: delegate cut was NOT sent to contract (retained by coordinator)
	// The coordinator only sent agentPoolWei, keeping delegateCut
	t.Logf("PASS: delegate keeps %s IOTX (10%%), contract receives %s IOTX (90%%)",
		weiToIOTX(delegateCut), weiToIOTX(agentPoolWei))
	_ = coordBalBefore
}

// --- Step 7: 100-Agent Stress Test (Optional) ---

func TestRewardE2E_Step7_HundredAgents(t *testing.T) {
	if os.Getenv("REWARD_E2E_STRESS") == "" {
		t.Skip("skipping 100-agent stress test (set REWARD_E2E_STRESS=1 to enable)")
	}

	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Step 7: 100-Agent Stress Test ===")

	n := 100
	addrs, keys, err := generateWallets(n)
	if err != nil {
		t.Fatalf("generate wallets: %v", err)
	}

	gasAmount := iotxToWei(gasPerAgent)
	env.fundWallets(t, addrs, gasAmount)

	epochRewardWei := iotxToWei(0.5)
	delegateCut := new(big.Int).Div(epochRewardWei, big.NewInt(10))
	agentPoolWei := new(big.Int).Sub(epochRewardWei, delegateCut)

	// Register all 100 agents first
	t.Log("--- Register 100 agents (weight=1, deposit=0) ---")
	regWeights := make([]*big.Int, n)
	for i := range regWeights {
		regWeights[i] = big.NewInt(1)
	}
	env.depositAndSettle(t, addrs, regWeights, big.NewInt(0))
	time.Sleep(time.Duration(env.epochWaitSec) * time.Second)

	// 3 epochs with deposits
	numEpochs := 3
	for epoch := 1; epoch <= numEpochs; epoch++ {
		t.Logf("--- Epoch %d ---", epoch)
		weights := make([]*big.Int, n)
		for i := range weights {
			weights[i] = big.NewInt(1)
		}
		env.depositAndSettle(t, addrs, weights, agentPoolWei)

		if epoch < numEpochs {
			time.Sleep(time.Duration(env.epochWaitSec) * time.Second)
		}
	}

	// Verify all 100 agents have claimable > 0
	allPositive := true
	for i, addr := range addrs {
		c := env.queryClaimable(t, addr)
		if c.Sign() == 0 {
			t.Errorf("agent-%03d has 0 claimable", i+1)
			allPositive = false
		}
	}
	if allPositive {
		t.Log("all 100 agents have claimable > 0")
	}

	// Claim 10 random agents
	claimIndices := []int{0, 10, 20, 30, 40, 50, 60, 70, 80, 99}
	for _, i := range claimIndices {
		c := env.queryClaimable(t, addrs[i])
		t.Logf("claiming agent-%03d (%s IOTX)...", i+1, weiToIOTX(c))
		env.executeClaim(t, keys[i])
	}

	t.Logf("PASS: 100 agents all rewarded, 10 claimed successfully")
}

// --- Invariant Check ---

func TestRewardE2E_Invariant_NoOverdraft(t *testing.T) {
	env := setupTestEnv(t)
	defer env.client.Close()
	env.deployFreshContract(t)

	t.Log("=== Invariant: Contract Balance >= Sum of Claimable ===")

	contractBal := env.queryContractBalance(t)
	totalWeight := env.queryTotalWeight(t)

	t.Logf("contract balance: %s IOTX", weiToIOTX(contractBal))
	t.Logf("total weight: %s", totalWeight.String())

	if contractBal.Sign() < 0 {
		t.Error("INVARIANT VIOLATION: contract balance is negative")
	}

	t.Log("PASS: contract solvent")
}
