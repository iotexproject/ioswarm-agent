package main

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie/utils"
	"github.com/holiman/uint256"
)

// emptyCodeHash is the keccak256 of empty bytes, used as code hash for EOAs.
var emptyCodeHash = crypto.Keccak256Hash(nil)

// account holds in-memory account state for the EVM.
type account struct {
	balance  *uint256.Int
	nonce    uint64
	code     []byte
	codeHash common.Hash
	suicided bool
	isNew    bool
}

// storageChange records a single SSTORE mutation.
type storageChange struct {
	address  common.Address
	slot     common.Hash
	oldValue common.Hash
	newValue common.Hash
}

// snapshotEntry is a deep copy of state at a point in time.
type snapshotEntry struct {
	accounts          map[common.Address]*account
	storage           map[common.Address]map[common.Hash]common.Hash
	changes           []storageChange
	logs              []*types.Log
	refund            uint64
	accessedAddresses map[common.Address]bool
	accessedSlots     map[common.Address]map[common.Hash]bool
	transientStorage  map[common.Address]map[common.Hash]common.Hash
}

// MemStateDB implements the vm.StateDB interface backed by in-memory maps.
// It uses the iotex-core go-ethereum fork's interface signatures.
type MemStateDB struct {
	accounts         map[common.Address]*account
	storage          map[common.Address]map[common.Hash]common.Hash
	committedStorage map[common.Address]map[common.Hash]common.Hash // original values, never modified
	logs             []*types.Log
	changes          []storageChange
	snapshots        []snapshotEntry
	txHash           common.Hash
	txIndex          int
	logIndex         uint
	refund           uint64

	// Track accessed addresses/slots for EIP-2929
	accessedAddresses map[common.Address]bool
	accessedSlots     map[common.Address]map[common.Hash]bool

	// Transient storage (EIP-1153)
	transientStorage map[common.Address]map[common.Hash]common.Hash

	// Point cache for verkle tree ops
	pointCache *utils.PointCache
}

// NewMemStateDB creates a new in-memory StateDB populated from a task.
func NewMemStateDB(task *taskPackage) *MemStateDB {
	s := &MemStateDB{
		accounts:          make(map[common.Address]*account),
		storage:           make(map[common.Address]map[common.Hash]common.Hash),
		committedStorage:  make(map[common.Address]map[common.Hash]common.Hash),
		accessedAddresses: make(map[common.Address]bool),
		accessedSlots:     make(map[common.Address]map[common.Hash]bool),
		transientStorage:  make(map[common.Address]map[common.Hash]common.Hash),
		pointCache:        utils.NewPointCache(4),
	}

	// Load sender
	if task.Sender != nil {
		addr := common.HexToAddress(task.Sender.Address)
		bal, _ := new(big.Int).SetString(task.Sender.Balance, 10)
		if bal == nil || bal.Sign() < 0 {
			bal = big.NewInt(0)
		}
		balU256, _ := uint256.FromBig(bal)
		if balU256 == nil {
			balU256 = new(uint256.Int)
		}
		s.accounts[addr] = &account{
			balance:  balU256,
			nonce:    task.Sender.Nonce,
			codeHash: emptyCodeHash,
		}
	}

	// Load receiver
	if task.Receiver != nil {
		addr := common.HexToAddress(task.Receiver.Address)
		bal, _ := new(big.Int).SetString(task.Receiver.Balance, 10)
		if bal == nil || bal.Sign() < 0 {
			bal = big.NewInt(0)
		}
		balU256, _ := uint256.FromBig(bal)
		if balU256 == nil {
			balU256 = new(uint256.Int)
		}
		acct := &account{
			balance:  balU256,
			nonce:    task.Receiver.Nonce,
			codeHash: emptyCodeHash,
		}
		if len(task.Receiver.CodeHash) > 0 {
			acct.codeHash = common.BytesToHash(task.Receiver.CodeHash)
		}
		s.accounts[addr] = acct
	}

	// Load contract code
	for addrHex, code := range task.ContractCode {
		addr := common.HexToAddress(addrHex)
		h := crypto.Keccak256Hash(code)
		if acct, ok := s.accounts[addr]; ok {
			acct.code = code
			acct.codeHash = h
		} else {
			s.accounts[addr] = &account{
				balance:  new(uint256.Int),
				code:     code,
				codeHash: h,
			}
		}
	}

	// Load storage slots
	for addrHex, slots := range task.StorageSlots {
		addr := common.HexToAddress(addrHex)
		if s.storage[addr] == nil {
			s.storage[addr] = make(map[common.Hash]common.Hash)
		}
		if s.committedStorage[addr] == nil {
			s.committedStorage[addr] = make(map[common.Hash]common.Hash)
		}
		for slotHex, valHex := range slots {
			k := common.HexToHash(slotHex)
			v := common.HexToHash(valHex)
			s.storage[addr][k] = v
			s.committedStorage[addr][k] = v // committed = initial
		}
	}

	return s
}

func (s *MemStateDB) getOrCreateAccount(addr common.Address) *account {
	if acct, ok := s.accounts[addr]; ok {
		return acct
	}
	acct := &account{balance: new(uint256.Int), isNew: true, codeHash: emptyCodeHash}
	s.accounts[addr] = acct
	return acct
}

// --- vm.StateDB interface implementation (iotex go-ethereum fork) ---

func (s *MemStateDB) CreateAccount(addr common.Address) {
	s.getOrCreateAccount(addr)
}

func (s *MemStateDB) CreateContract(addr common.Address) {
	acct := s.getOrCreateAccount(addr)
	acct.isNew = true
}

func (s *MemStateDB) IsNewAccount(addr common.Address) bool {
	acct, ok := s.accounts[addr]
	if !ok {
		return true // non-existent account is considered "new"
	}
	return acct.isNew
}

func (s *MemStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acct := s.getOrCreateAccount(addr)
	prev := *acct.balance
	if acct.balance.Cmp(amount) < 0 {
		// Underflow: clamp to zero (canTransfer should prevent this in normal flow)
		acct.balance = new(uint256.Int)
	} else {
		acct.balance = new(uint256.Int).Sub(acct.balance, amount)
	}
	return prev
}

func (s *MemStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acct := s.getOrCreateAccount(addr)
	prev := *acct.balance
	acct.balance = new(uint256.Int).Add(acct.balance, amount)
	return prev
}

func (s *MemStateDB) GetBalance(addr common.Address) *uint256.Int {
	if acct, ok := s.accounts[addr]; ok {
		return acct.balance.Clone()
	}
	return new(uint256.Int)
}

func (s *MemStateDB) GetNonce(addr common.Address) uint64 {
	if acct, ok := s.accounts[addr]; ok {
		return acct.nonce
	}
	return 0
}

func (s *MemStateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	s.getOrCreateAccount(addr).nonce = nonce
}

func (s *MemStateDB) GetCodeHash(addr common.Address) common.Hash {
	if acct, ok := s.accounts[addr]; ok {
		return acct.codeHash
	}
	return common.Hash{} // non-existent account
}

func (s *MemStateDB) GetCode(addr common.Address) []byte {
	if acct, ok := s.accounts[addr]; ok {
		return acct.code
	}
	return nil
}

func (s *MemStateDB) SetCode(addr common.Address, code []byte) []byte {
	acct := s.getOrCreateAccount(addr)
	prev := acct.code
	acct.code = code
	if len(code) > 0 {
		acct.codeHash = crypto.Keccak256Hash(code)
	} else {
		acct.codeHash = emptyCodeHash
	}
	return prev
}

func (s *MemStateDB) GetCodeSize(addr common.Address) int {
	return len(s.GetCode(addr))
}

func (s *MemStateDB) AddRefund(gas uint64) {
	s.refund += gas
}

func (s *MemStateDB) SubRefund(gas uint64) {
	if gas > s.refund {
		panic("Refund counter below zero")
	}
	s.refund -= gas
}

func (s *MemStateDB) GetRefund() uint64 {
	return s.refund
}

func (s *MemStateDB) GetCommittedState(addr common.Address, slot common.Hash) common.Hash {
	if stor, ok := s.committedStorage[addr]; ok {
		return stor[slot]
	}
	return common.Hash{}
}

func (s *MemStateDB) GetState(addr common.Address, slot common.Hash) common.Hash {
	if stor, ok := s.storage[addr]; ok {
		return stor[slot]
	}
	return common.Hash{}
}

func (s *MemStateDB) SetState(addr common.Address, slot common.Hash, value common.Hash) common.Hash {
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	old := s.storage[addr][slot]
	s.storage[addr][slot] = value

	s.changes = append(s.changes, storageChange{
		address:  addr,
		slot:     slot,
		oldValue: old,
		newValue: value,
	})
	return old
}

func (s *MemStateDB) GetStorageRoot(addr common.Address) common.Hash {
	return common.Hash{}
}

func (s *MemStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if m, ok := s.transientStorage[addr]; ok {
		return m[key]
	}
	return common.Hash{}
}

func (s *MemStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	if s.transientStorage[addr] == nil {
		s.transientStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.transientStorage[addr][key] = value
}

func (s *MemStateDB) SelfDestruct(addr common.Address) uint256.Int {
	if acct, ok := s.accounts[addr]; ok {
		prev := *acct.balance
		acct.suicided = true
		acct.balance = new(uint256.Int)
		return prev
	}
	return uint256.Int{}
}

func (s *MemStateDB) HasSelfDestructed(addr common.Address) bool {
	if acct, ok := s.accounts[addr]; ok {
		return acct.suicided
	}
	return false
}

func (s *MemStateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	if acct, ok := s.accounts[addr]; ok && acct.isNew {
		prev := s.SelfDestruct(addr)
		return prev, true
	}
	if acct, ok := s.accounts[addr]; ok {
		prev := *acct.balance
		acct.balance = new(uint256.Int)
		return prev, false
	}
	return uint256.Int{}, false
}

func (s *MemStateDB) Exist(addr common.Address) bool {
	_, ok := s.accounts[addr]
	return ok
}

func (s *MemStateDB) Empty(addr common.Address) bool {
	acct, ok := s.accounts[addr]
	if !ok {
		return true
	}
	return acct.nonce == 0 && acct.balance.IsZero() && len(acct.code) == 0
}

func (s *MemStateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessedAddresses[addr]
}

func (s *MemStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	addressOk = s.accessedAddresses[addr]
	if m, ok := s.accessedSlots[addr]; ok {
		slotOk = m[slot]
	}
	return
}

func (s *MemStateDB) AddAddressToAccessList(addr common.Address) {
	s.accessedAddresses[addr] = true
}

func (s *MemStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	s.accessedAddresses[addr] = true
	if s.accessedSlots[addr] == nil {
		s.accessedSlots[addr] = make(map[common.Hash]bool)
	}
	s.accessedSlots[addr][slot] = true
}

func (s *MemStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessedAddresses = make(map[common.Address]bool)
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool)

	s.AddAddressToAccessList(sender)
	s.AddAddressToAccessList(coinbase)
	if dest != nil {
		s.AddAddressToAccessList(*dest)
	}
	for _, addr := range precompiles {
		s.AddAddressToAccessList(addr)
	}
	for _, entry := range txAccesses {
		s.AddAddressToAccessList(entry.Address)
		for _, slot := range entry.StorageKeys {
			s.AddSlotToAccessList(entry.Address, slot)
		}
	}
}

func (s *MemStateDB) Snapshot() int {
	snap := snapshotEntry{
		accounts: make(map[common.Address]*account, len(s.accounts)),
		storage:  make(map[common.Address]map[common.Hash]common.Hash, len(s.storage)),
		changes:  make([]storageChange, len(s.changes)),
		logs:     make([]*types.Log, len(s.logs)),
		refund:   s.refund,
	}

	// Deep copy accounts
	for addr, acct := range s.accounts {
		snap.accounts[addr] = &account{
			balance:  acct.balance.Clone(),
			nonce:    acct.nonce,
			code:     append([]byte(nil), acct.code...),
			codeHash: acct.codeHash,
			suicided: acct.suicided,
			isNew:    acct.isNew,
		}
	}
	// Deep copy storage
	for addr, slots := range s.storage {
		snap.storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		for k, v := range slots {
			snap.storage[addr][k] = v
		}
	}
	copy(snap.changes, s.changes)
	copy(snap.logs, s.logs)

	// Deep copy access lists
	snap.accessedAddresses = make(map[common.Address]bool, len(s.accessedAddresses))
	for k, v := range s.accessedAddresses {
		snap.accessedAddresses[k] = v
	}
	snap.accessedSlots = make(map[common.Address]map[common.Hash]bool, len(s.accessedSlots))
	for addr, slots := range s.accessedSlots {
		snap.accessedSlots[addr] = make(map[common.Hash]bool, len(slots))
		for k, v := range slots {
			snap.accessedSlots[addr][k] = v
		}
	}

	// Deep copy transient storage (EIP-1153)
	snap.transientStorage = make(map[common.Address]map[common.Hash]common.Hash, len(s.transientStorage))
	for addr, slots := range s.transientStorage {
		snap.transientStorage[addr] = make(map[common.Hash]common.Hash, len(slots))
		for k, v := range slots {
			snap.transientStorage[addr][k] = v
		}
	}

	id := len(s.snapshots)
	s.snapshots = append(s.snapshots, snap)
	return id
}

func (s *MemStateDB) RevertToSnapshot(id int) {
	if id < 0 || id >= len(s.snapshots) {
		return
	}
	snap := s.snapshots[id]

	// Deep copy FROM snapshot (so snapshot data is preserved for potential re-revert)
	s.accounts = make(map[common.Address]*account, len(snap.accounts))
	for addr, acct := range snap.accounts {
		s.accounts[addr] = &account{
			balance:  acct.balance.Clone(),
			nonce:    acct.nonce,
			code:     append([]byte(nil), acct.code...),
			codeHash: acct.codeHash,
			suicided: acct.suicided,
			isNew:    acct.isNew,
		}
	}
	s.storage = make(map[common.Address]map[common.Hash]common.Hash, len(snap.storage))
	for addr, slots := range snap.storage {
		s.storage[addr] = make(map[common.Hash]common.Hash, len(slots))
		for k, v := range slots {
			s.storage[addr][k] = v
		}
	}

	s.changes = make([]storageChange, len(snap.changes))
	copy(s.changes, snap.changes)
	s.logs = make([]*types.Log, len(snap.logs))
	copy(s.logs, snap.logs)
	s.refund = snap.refund

	// Restore access lists
	s.accessedAddresses = make(map[common.Address]bool, len(snap.accessedAddresses))
	for k, v := range snap.accessedAddresses {
		s.accessedAddresses[k] = v
	}
	s.accessedSlots = make(map[common.Address]map[common.Hash]bool, len(snap.accessedSlots))
	for addr, slots := range snap.accessedSlots {
		s.accessedSlots[addr] = make(map[common.Hash]bool, len(slots))
		for k, v := range slots {
			s.accessedSlots[addr][k] = v
		}
	}

	// Restore transient storage (EIP-1153)
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash, len(snap.transientStorage))
	for addr, slots := range snap.transientStorage {
		s.transientStorage[addr] = make(map[common.Hash]common.Hash, len(slots))
		for k, v := range slots {
			s.transientStorage[addr][k] = v
		}
	}

	s.snapshots = s.snapshots[:id]
}

func (s *MemStateDB) AddLog(log *types.Log) {
	log.TxHash = s.txHash
	log.TxIndex = uint(s.txIndex)
	log.Index = s.logIndex
	s.logIndex++
	s.logs = append(s.logs, log)
}

func (s *MemStateDB) AddPreimage(_ common.Hash, _ []byte) {}

func (s *MemStateDB) PointCache() *utils.PointCache {
	return s.pointCache
}

func (s *MemStateDB) Witness() *stateless.Witness {
	return nil
}

func (s *MemStateDB) AccessEvents() *state.AccessEvents {
	return state.NewAccessEvents(s.pointCache)
}

func (s *MemStateDB) Finalise(_ bool) {
	// No-op for in-memory state
}

// --- State diff extraction ---

// GetStateChanges returns all storage mutations recorded during execution.
func (s *MemStateDB) GetStateChanges() []*stateChange {
	type key struct {
		addr common.Address
		slot common.Hash
	}
	seen := make(map[key]int)
	var result []*stateChange

	for _, c := range s.changes {
		k := key{c.address, c.slot}
		sc := &stateChange{
			Address:  c.address.Hex(),
			Slot:     c.slot.Hex(),
			OldValue: c.oldValue.Hex(),
			NewValue: c.newValue.Hex(),
		}
		if idx, ok := seen[k]; ok {
			result[idx].NewValue = sc.NewValue
		} else {
			seen[k] = len(result)
			result = append(result, sc)
		}
	}

	// Filter out no-ops
	var filtered []*stateChange
	for _, sc := range result {
		if sc.OldValue != sc.NewValue {
			filtered = append(filtered, sc)
		}
	}
	return filtered
}

// GetLogs returns all logs emitted during execution.
func (s *MemStateDB) GetLogs() []*types.Log {
	return s.logs
}
