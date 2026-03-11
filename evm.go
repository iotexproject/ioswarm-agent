package main

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// _inContractTransfer is the topic hash used by iotex-core's MakeTransfer
// to emit synthetic logs for internal value transfers. This MUST match
// the constant in iotex-core/action/protocol/execution/evm/evm.go.
//
// In iotex-core: hash.BytesToHash256([]byte{byte(iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER)})
// IN_CONTRACT_TRANSFER = 0, and BytesToHash256 right-pads to 32 bytes → all zeros.
var _inContractTransfer = make([]byte, 32)

// evmResult holds the outcome of EVM execution.
type evmResult struct {
	Success      bool
	GasUsed      uint64
	ReturnData   []byte
	StateChanges []*stateChange
	Logs         []*logEntry
	Error        string
}

// iotexChainConfig returns the chain config matching iotex-core's getChainConfig().
// All Ethereum forks through London are activated at block 0 (IoTeX has its own
// hard-fork schedule that maps to these).
func iotexChainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(4689), // IoTeX mainnet
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
	}
}

// executeEVM runs a single EVM transaction against the task's state.
// It replicates iotex-core's executeInEVM() behavior including:
//   - IoTeX chain config (all forks through London at block 0)
//   - Difficulty = 50 (iotex-core hardcoded value)
//   - MakeTransfer with synthetic _inContractTransfer log
//   - Same go-ethereum fork (via replace directive in go.mod)
func executeEVM(task *taskPackage) (result *evmResult) {
	// Recover from go-ethereum panics (SubRefund, MustFromBig, etc.)
	defer func() {
		if r := recover(); r != nil {
			result = &evmResult{
				Success: false,
				Error:   fmt.Sprintf("EVM panic: %v", r),
			}
		}
	}()

	if task.EvmTx == nil {
		return &evmResult{
			Success: false,
			Error:   "no EVM tx data",
		}
	}

	if task.Sender == nil {
		return &evmResult{
			Success: false,
			Error:   "no sender account state",
		}
	}

	// 1. Build MemStateDB from task
	stateDB := NewMemStateDB(task)

	// 2. Parse transaction fields
	etx := task.EvmTx
	gasPrice, _ := new(big.Int).SetString(etx.GasPrice, 10)
	if gasPrice == nil {
		gasPrice = big.NewInt(0)
	}
	value, _ := new(big.Int).SetString(etx.Value, 10)
	if value == nil {
		value = big.NewInt(0)
	}

	senderAddr := common.HexToAddress(task.Sender.Address)

	// 3. Build block context (matching iotex-core's executeInEVM)
	var blockCtx vm.BlockContext
	if task.BlockContext != nil {
		baseFee, _ := new(big.Int).SetString(task.BlockContext.BaseFee, 10)
		if baseFee == nil {
			baseFee = big.NewInt(0)
		}
		blockCtx = vm.BlockContext{
			CanTransfer: canTransfer,
			Transfer:    iotexTransfer, // iotex-core's MakeTransfer
			GetHash:     mockGetHash,
			Coinbase:    common.HexToAddress(task.BlockContext.Coinbase),
			BlockNumber: new(big.Int).SetUint64(task.BlockContext.Number),
			Time:        task.BlockContext.Timestamp,
			Difficulty:  new(big.Int).SetUint64(50), // iotex-core hardcoded
			GasLimit:    task.BlockContext.GasLimit,
			BaseFee:     baseFee,
		}
	} else {
		blockCtx = vm.BlockContext{
			CanTransfer: canTransfer,
			Transfer:    iotexTransfer,
			GetHash:     mockGetHash,
			BlockNumber: new(big.Int).SetUint64(task.BlockHeight),
			Time:        0,
			Difficulty:  new(big.Int).SetUint64(50),
			GasLimit:    30_000_000,
			BaseFee:     big.NewInt(0),
		}
	}

	// 4. Build tx context
	txCtx := vm.TxContext{
		Origin:   senderAddr,
		GasPrice: gasPrice,
	}

	// 5. Create EVM with IoTeX chain config
	chainCfg := iotexChainConfig()
	evm := vm.NewEVM(blockCtx, stateDB, chainCfg, vm.Config{})
	evm.SetTxContext(txCtx)

	// 6. Execute (iotex fork uses common.Address, not AccountRef)
	var (
		ret     []byte
		gasLeft uint64
		err     error
	)

	gas := etx.GasLimit
	valueU256, overflow := uint256.FromBig(value)
	if overflow {
		return &evmResult{
			Success: false,
			Error:   "tx value exceeds uint256",
		}
	}

	if etx.To == "" {
		// Contract creation
		ret, _, gasLeft, err = evm.Create(senderAddr, etx.Data, gas, valueU256)
	} else {
		toAddr := common.HexToAddress(etx.To)
		ret, gasLeft, err = evm.Call(senderAddr, toAddr, etx.Data, gas, valueU256)
	}

	gasUsed := gas - gasLeft

	// 7. Collect results
	result = &evmResult{
		Success:      err == nil,
		GasUsed:      gasUsed,
		ReturnData:   ret,
		StateChanges: stateDB.GetStateChanges(),
	}

	// Convert logs (filter out synthetic _inContractTransfer logs)
	for _, log := range stateDB.GetLogs() {
		le := &logEntry{
			Address: log.Address.Hex(),
			Data:    log.Data,
		}
		for _, topic := range log.Topics {
			le.Topics = append(le.Topics, topic.Hex())
		}
		result.Logs = append(result.Logs, le)
	}

	if err != nil {
		result.Error = fmt.Sprintf("%v", err)
	}

	return result
}

// canTransfer checks if the account has enough balance.
func canTransfer(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

// iotexTransfer replicates iotex-core's MakeTransfer function.
// It moves value between accounts AND emits a synthetic log with the
// _inContractTransfer topic, matching the delegate's behavior exactly.
func iotexTransfer(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
	db.SubBalance(sender, amount, tracing.BalanceChangeUnspecified)
	db.AddBalance(recipient, amount, tracing.BalanceChangeUnspecified)
	db.AddLog(&types.Log{
		Topics: []common.Hash{
			common.BytesToHash(_inContractTransfer),
			common.BytesToHash(sender[:]),
			common.BytesToHash(recipient[:]),
		},
		Data: amount.Bytes(),
	})
}

// mockGetHash returns a deterministic hash for block numbers.
func mockGetHash(n uint64) common.Hash {
	return common.BigToHash(new(big.Int).SetUint64(n))
}
