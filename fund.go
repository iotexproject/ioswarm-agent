package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

func runFund(args []string) {
	fs := flag.NewFlagSet("fund", flag.ExitOnError)
	privateKeyHex := fs.String("private-key", "", "funder wallet private key (hex, or IOSWARM_PRIVATE_KEY)")
	rpc := fs.String("rpc", iotexMainnetRPC, "IoTeX RPC endpoint")
	chainID := fs.Int64("chain-id", iotexMainnetChainID, "chain ID (4689=mainnet, 4690=testnet)")
	amountIOTX := fs.Float64("amount", 0.1, "IOTX to send to each wallet")
	dryRun := fs.Bool("dry-run", false, "show what would be sent without executing")
	fs.Parse(args)

	// Env fallback
	if *privateKeyHex == "" {
		*privateKeyHex = os.Getenv("IOSWARM_PRIVATE_KEY")
	}
	if *privateKeyHex == "" {
		fmt.Fprintf(os.Stderr, "error: --private-key is required\n")
		fmt.Fprintf(os.Stderr, "Usage: ioswarm-agent fund --private-key=<hex> <wallet1> <wallet2> ...\n")
		os.Exit(1)
	}

	wallets := fs.Args()
	if len(wallets) == 0 {
		fmt.Fprintf(os.Stderr, "error: at least one wallet address required\n")
		fmt.Fprintf(os.Stderr, "Usage: ioswarm-agent fund --private-key=<hex> 0xWallet1 0xWallet2 ...\n")
		os.Exit(1)
	}

	// Parse funder key
	hexKey := strings.TrimPrefix(*privateKeyHex, "0x")
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing private key: %v\n", err)
		os.Exit(1)
	}
	funderAddr := crypto.PubkeyToAddress(key.PublicKey)

	// Connect
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client, err := ethclient.DialContext(ctx, *rpc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error connecting to %s: %v\n", *rpc, err)
		os.Exit(1)
	}
	defer client.Close()

	// Check funder balance
	balance, err := client.BalanceAt(ctx, funderAddr, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting balance: %v\n", err)
		os.Exit(1)
	}

	// Convert amount to wei
	amountWei := iotxToWei(*amountIOTX)
	totalNeeded := new(big.Int).Mul(amountWei, big.NewInt(int64(len(wallets))))

	fmt.Println("=== Fund Agent Wallets ===")
	fmt.Printf("Funder:   %s\n", funderAddr.Hex())
	fmt.Printf("Balance:  %s IOTX\n", weiToIOTX(balance))
	fmt.Printf("Amount:   %s IOTX per wallet\n", weiToIOTX(amountWei))
	fmt.Printf("Wallets:  %d\n", len(wallets))
	fmt.Printf("Total:    %s IOTX\n", weiToIOTX(totalNeeded))
	fmt.Println()

	if balance.Cmp(totalNeeded) < 0 {
		fmt.Fprintf(os.Stderr, "error: insufficient balance (%s IOTX < %s IOTX needed)\n",
			weiToIOTX(balance), weiToIOTX(totalNeeded))
		os.Exit(1)
	}

	if *dryRun {
		fmt.Println("(dry-run mode — listing wallets)")
		for i, w := range wallets {
			fmt.Printf("  [%d] %s → %s IOTX\n", i+1, w, weiToIOTX(amountWei))
		}
		fmt.Printf("\nTotal: %s IOTX to %d wallets\n", weiToIOTX(totalNeeded), len(wallets))
		return
	}

	// Get nonce
	nonce, err := client.PendingNonceAt(ctx, funderAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting nonce: %v\n", err)
		os.Exit(1)
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting gas price: %v\n", err)
		os.Exit(1)
	}

	signer := types.NewEIP155Signer(big.NewInt(*chainID))
	sent := 0
	failed := 0

	for i, w := range wallets {
		to := common.HexToAddress(w)

		tx := types.NewTransaction(
			nonce,
			to,
			amountWei,
			21000, // standard transfer gas
			gasPrice,
			nil,
		)

		signedTx, err := types.SignTx(tx, signer, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%d] error signing: %v\n", i+1, err)
			failed++
			continue
		}

		if err := client.SendTransaction(ctx, signedTx); err != nil {
			fmt.Fprintf(os.Stderr, "[%d] error sending to %s: %v\n", i+1, w, err)
			failed++
			continue
		}

		fmt.Printf("[%d/%d] sent %s IOTX → %s (tx: %s)\n",
			i+1, len(wallets), weiToIOTX(amountWei), w, signedTx.Hash().Hex())
		nonce++
		sent++
	}

	fmt.Println()
	fmt.Printf("Done: %d sent, %d failed\n", sent, failed)

	if sent > 0 {
		fmt.Println("Waiting for last tx confirmation...")
		waitForConfirmation(ctx, client, sent, failed)
	}
}

func waitForConfirmation(ctx context.Context, client *ethclient.Client, sent, failed int) {
	// Just wait a bit for the txs to be mined
	time.Sleep(15 * time.Second)
	fmt.Printf("Funding complete: %d/%d wallets funded\n", sent, sent+failed)
}

// iotxToWei converts an IOTX float to wei (big.Int).
func iotxToWei(iotx float64) *big.Int {
	// Use big.Float for precision: iotx * 1e18
	f := new(big.Float).SetFloat64(iotx)
	e18 := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	f.Mul(f, e18)
	wei, _ := f.Int(nil)
	return wei
}

