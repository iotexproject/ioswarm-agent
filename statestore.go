package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
	bolt "go.etcd.io/bbolt"
)

// StateStore provides persistent EVM state storage using BoltDB.
// Bucket structure matches iotex-core's namespaces:
//
//	"Account"  → key: addr_bytes → value: serialized account state
//	"Code"     → key: codeHash   → value: contract bytecode
//	"Contract" → key: addr+slot  → value: storage value
//	"_meta"    → key: "height"   → value: uint64 big-endian
type StateStore struct {
	db     *bolt.DB
	height atomic.Uint64
	logger *zap.Logger
}

// Well-known namespace strings matching iotex-core's constants.
const (
	nsAccount  = "Account"
	nsCode     = "Code"
	nsContract = "Contract"
	nsMeta     = "_meta"
	metaHeight = "height"
)

// WriteType constants for state diff entries.
const (
	WriteTypePut    uint8 = 0
	WriteTypeDelete uint8 = 1
)

// OpenStateStore opens or creates a BoltDB state store at the given path.
func OpenStateStore(path string, logger *zap.Logger) (*StateStore, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{
		NoSync:         false,
		NoFreelistSync: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}

	s := &StateStore{db: db, logger: logger}

	// Ensure buckets exist and load height
	err = db.Update(func(tx *bolt.Tx) error {
		for _, ns := range []string{nsAccount, nsCode, nsContract, nsMeta} {
			if _, err := tx.CreateBucketIfNotExists([]byte(ns)); err != nil {
				return err
			}
		}
		// Load persisted height
		if b := tx.Bucket([]byte(nsMeta)); b != nil {
			if v := b.Get([]byte(metaHeight)); len(v) == 8 {
				s.height.Store(binary.BigEndian.Uint64(v))
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}

	logger.Info("state store opened",
		zap.String("path", path),
		zap.Uint64("height", s.height.Load()))
	return s, nil
}

// Close closes the BoltDB.
func (s *StateStore) Close() error {
	return s.db.Close()
}

// Height returns the current synced height.
func (s *StateStore) Height() uint64 {
	return s.height.Load()
}

// ApplyDiff applies a single block's state diff entries atomically.
// Also updates both the persisted height in BoltDB and the in-memory atomic.
func (s *StateStore) ApplyDiff(height uint64, entries []*stateDiffEntry) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		for _, e := range entries {
			bucket := tx.Bucket([]byte(e.Namespace))
			if bucket == nil {
				var createErr error
				bucket, createErr = tx.CreateBucketIfNotExists([]byte(e.Namespace))
				if createErr != nil {
					return fmt.Errorf("create bucket %s: %w", e.Namespace, createErr)
				}
			}
			switch e.WriteType {
			case WriteTypePut:
				if err := bucket.Put(e.Key, e.Value); err != nil {
					return err
				}
			case WriteTypeDelete:
				if err := bucket.Delete(e.Key); err != nil {
					return err
				}
			default:
				s.logger.Warn("unknown write type in state diff",
					zap.Uint8("write_type", e.WriteType),
					zap.String("namespace", e.Namespace))
			}
		}
		// Update height in DB
		hBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(hBuf, height)
		return tx.Bucket([]byte(nsMeta)).Put([]byte(metaHeight), hBuf)
	})
	if err == nil {
		// Update in-memory height atomically after successful DB write
		s.height.Store(height)
	}
	return err
}

// Get reads a value by namespace and key. Returns nil if not found.
func (s *StateStore) Get(namespace string, key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespace))
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v != nil {
			val = make([]byte, len(v))
			copy(val, v)
		}
		return nil
	})
	return val, err
}

// GetAccount reads and decodes an IoTeX Account from the Account namespace.
// addrHash is the 20-byte address (bech32-decoded for io1, hex-decoded for 0x).
// Returns nil, nil if the account is not found.
func (s *StateStore) GetAccount(addrHash []byte) (*IoTXAccount, error) {
	data, err := s.Get(nsAccount, addrHash)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return DecodeAccount(data)
}

// GetCode reads contract bytecode from the Code namespace.
// codeHash is the hash of the contract bytecode.
// Returns nil, nil if the code is not found.
func (s *StateStore) GetCode(codeHash []byte) ([]byte, error) {
	return s.Get(nsCode, codeHash)
}

// GetStorageSlot looks up a contract storage slot by traversing the local MPT trie.
// addrHash is the 20-byte address (Account namespace key).
// slot is the 32-byte storage key as used by EVM (SLOAD/SSTORE operand).
// Returns the raw storage value (typically 32 bytes), or nil if not found.
func (s *StateStore) GetStorageSlot(addrHash []byte, slot []byte) ([]byte, error) {
	acct, err := s.GetAccount(addrHash)
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	if acct == nil {
		return nil, nil
	}
	if len(acct.Root) == 0 {
		return nil, nil
	}
	val, err := trieGet(s, acct.Root, slot)
	if err != nil {
		if errors.Is(err, errKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("trie get: %w", err)
	}
	return val, nil
}

// Stats returns basic stats about the store.
func (s *StateStore) Stats() map[string]int {
	stats := make(map[string]int)
	_ = s.db.View(func(tx *bolt.Tx) error {
		for _, ns := range []string{nsAccount, nsCode, nsContract} {
			b := tx.Bucket([]byte(ns))
			if b != nil {
				stats[ns] = b.Stats().KeyN
			}
		}
		return nil
	})
	return stats
}
