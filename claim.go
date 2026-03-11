package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	iotexMainnetRPC     = "https://babel-api.mainnet.iotex.io"
	iotexMainnetChainID = 4689
	iotexTestnetRPC     = "https://babel-api.testnet.iotex.io"
	iotexTestnetChainID = 4690
)

// Minimal ABI for AgentRewardPool (claim + claimable only)
const rewardPoolABI = `[{"inputs":[],"name":"claim","outputs":[],"stateMutability":"nonpayable","type":"function"},{"inputs":[{"internalType":"address","name":"agent","type":"address"}],"name":"claimable","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"}]`

func runClaim(args []string) {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	contractAddr := fs.String("contract", "", "AgentRewardPool contract address (0x... or io1...)")
	privateKeyHex := fs.String("private-key", "", "agent wallet private key (hex, or set IOSWARM_PRIVATE_KEY)")
	rpc := fs.String("rpc", iotexMainnetRPC, "IoTeX RPC endpoint")
	chainID := fs.Int64("chain-id", iotexMainnetChainID, "chain ID (4689=mainnet, 4690=testnet)")
	dryRun := fs.Bool("dry-run", false, "only show claimable amount, don't send tx")
	fs.Parse(args)

	// Env fallbacks
	if *privateKeyHex == "" {
		*privateKeyHex = os.Getenv("IOSWARM_PRIVATE_KEY")
	}
	if *contractAddr == "" {
		*contractAddr = os.Getenv("IOSWARM_REWARD_CONTRACT")
	}

	if *contractAddr == "" {
		fmt.Fprintf(os.Stderr, "error: --contract is required\n")
		fmt.Fprintf(os.Stderr, "Usage: ioswarm-agent claim --contract=0x... --private-key=<hex>\n")
		os.Exit(1)
	}
	if *privateKeyHex == "" && !*dryRun {
		fmt.Fprintf(os.Stderr, "error: --private-key is required (or use --dry-run to check balance)\n")
		os.Exit(1)
	}

	// Parse contract address
	contract := common.HexToAddress(*contractAddr)
	if !strings.HasPrefix(*contractAddr, "0x") && !strings.HasPrefix(*contractAddr, "0X") {
		fmt.Fprintf(os.Stderr, "warning: contract address should be 0x format (not io1... for EVM calls)\n")
	}

	// Parse ABI
	parsedABI, err := abi.JSON(strings.NewReader(rewardPoolABI))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing ABI: %v\n", err)
		os.Exit(1)
	}

	// Connect to RPC
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := ethclient.DialContext(ctx, *rpc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error connecting to %s: %v\n", *rpc, err)
		os.Exit(1)
	}
	defer client.Close()

	// If dry-run with no key, we need at least a wallet address
	var key *ecdsa.PrivateKey
	var walletAddr common.Address

	if *privateKeyHex != "" {
		// Strip 0x prefix if present
		hexKey := strings.TrimPrefix(*privateKeyHex, "0x")
		key, err = crypto.HexToECDSA(hexKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error parsing private key: %v\n", err)
			os.Exit(1)
		}
		walletAddr = crypto.PubkeyToAddress(key.PublicKey)
	} else {
		// dry-run without key — need wallet flag
		fmt.Fprintf(os.Stderr, "error: --private-key required even for dry-run (to derive wallet address)\n")
		os.Exit(1)
	}

	fmt.Printf("Wallet:   %s\n", walletAddr.Hex())
	fmt.Printf("Contract: %s\n", contract.Hex())
	fmt.Printf("RPC:      %s\n", *rpc)
	fmt.Println()

	// 1. Check claimable amount
	claimableData, err := parsedABI.Pack("claimable", walletAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error packing claimable: %v\n", err)
		os.Exit(1)
	}

	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &contract,
		Data: claimableData,
	}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error calling claimable(): %v\n", err)
		os.Exit(1)
	}

	outputs, err := parsedABI.Unpack("claimable", result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error unpacking claimable result: %v\n", err)
		os.Exit(1)
	}

	claimable := outputs[0].(*big.Int)
	fmt.Printf("Claimable: %s IOTX (%s rau)\n", weiToIOTX(claimable), claimable.String())

	if claimable.Sign() == 0 {
		fmt.Println("\nNothing to claim.")
		return
	}

	if *dryRun {
		fmt.Println("\n(dry-run mode — not sending transaction)")
		return
	}

	// 2. Send claim() transaction
	fmt.Println("\nSending claim() transaction...")

	claimCalldata, err := parsedABI.Pack("claim")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error packing claim: %v\n", err)
		os.Exit(1)
	}

	nonce, err := client.PendingNonceAt(ctx, walletAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting nonce: %v\n", err)
		os.Exit(1)
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting gas price: %v\n", err)
		os.Exit(1)
	}

	tx := types.NewTransaction(
		nonce,
		contract,
		big.NewInt(0), // no IOTX sent with claim
		200000,        // gas limit
		gasPrice,
		claimCalldata,
	)

	signer := types.NewEIP155Signer(big.NewInt(*chainID))
	signedTx, err := types.SignTx(tx, signer, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error signing tx: %v\n", err)
		os.Exit(1)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		fmt.Fprintf(os.Stderr, "error sending tx: %v\n", err)
		os.Exit(1)
	}

	txHash := signedTx.Hash()
	fmt.Printf("Tx sent: %s\n", txHash.Hex())
	fmt.Println("Waiting for confirmation...")

	// 3. Wait for receipt
	for i := 0; i < 30; i++ {
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if receipt.Status == 1 {
			fmt.Printf("\nClaimed %s IOTX successfully!\n", weiToIOTX(claimable))
			fmt.Printf("Block:    %d\n", receipt.BlockNumber.Uint64())
			fmt.Printf("Gas used: %d\n", receipt.GasUsed)
		} else {
			fmt.Fprintf(os.Stderr, "\nTransaction reverted! Gas used: %d\n", receipt.GasUsed)
			os.Exit(1)
		}
		return
	}

	fmt.Println("\nTx submitted but not yet confirmed. Check manually:")
	fmt.Printf("  https://iotexscan.io/tx/%s\n", txHash.Hex())
}

func weiToIOTX(wei *big.Int) string {
	if wei.Sign() == 0 {
		return "0"
	}
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	f := new(big.Float).SetInt(wei)
	e := new(big.Float).SetInt(one)
	f.Quo(f, e)
	return f.Text('f', 6)
}
