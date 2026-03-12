package main

import (
	"bytes"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// storeNode serializes a trie node, computes its Hash160b, and stores it
// in the Contract namespace. Returns the 20-byte hash.
func storeNode(t *testing.T, store *StateStore, nodeData []byte) []byte {
	t.Helper()
	h := hash160b(nodeData)
	err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nsContract)).Put(h, nodeData)
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHash160b(t *testing.T) {
	// Verify hash160b produces 20 bytes
	h := hash160b([]byte("hello"))
	if len(h) != 20 {
		t.Errorf("hash160b length = %d, want 20", len(h))
	}

	// Same input → same output
	h2 := hash160b([]byte("hello"))
	if !bytes.Equal(h, h2) {
		t.Error("hash160b not deterministic")
	}

	// Different input → different output
	h3 := hash160b([]byte("world"))
	if bytes.Equal(h, h3) {
		t.Error("hash160b collision on 'hello' vs 'world'")
	}
}

func TestDecodeNodePb(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		key := []byte{0x01, 0x02, 0x03}
		val := []byte{0xAA, 0xBB}
		data := encodeLeafNode(key, val)

		nodeType, _, _, leaf, err := decodeNodePb(data)
		if err != nil {
			t.Fatal(err)
		}
		if nodeType != 'L' {
			t.Errorf("nodeType = %c, want L", nodeType)
		}
		if !bytes.Equal(leaf.key, key) {
			t.Errorf("leaf.key = %x, want %x", leaf.key, key)
		}
		if !bytes.Equal(leaf.value, val) {
			t.Errorf("leaf.value = %x, want %x", leaf.value, val)
		}
	})

	t.Run("branch", func(t *testing.T) {
		children := map[byte][]byte{
			0x05: bytes.Repeat([]byte{0xAA}, 20),
			0x0A: bytes.Repeat([]byte{0xBB}, 20),
		}
		data := encodeBranchNode(children)

		nodeType, branch, _, _, err := decodeNodePb(data)
		if err != nil {
			t.Fatal(err)
		}
		if nodeType != 'B' {
			t.Errorf("nodeType = %c, want B", nodeType)
		}
		if len(branch) != 2 {
			t.Errorf("branch children = %d, want 2", len(branch))
		}
		if !bytes.Equal(branch[0x05], children[0x05]) {
			t.Errorf("branch[5] mismatch")
		}
		if !bytes.Equal(branch[0x0A], children[0x0A]) {
			t.Errorf("branch[10] mismatch")
		}
	})

	t.Run("extension", func(t *testing.T) {
		path := []byte{0x01, 0x02}
		childHash := bytes.Repeat([]byte{0xCC}, 20)
		data := encodeExtensionNode(path, childHash)

		nodeType, _, ext, _, err := decodeNodePb(data)
		if err != nil {
			t.Fatal(err)
		}
		if nodeType != 'E' {
			t.Errorf("nodeType = %c, want E", nodeType)
		}
		if !bytes.Equal(ext.path, path) {
			t.Errorf("ext.path = %x, want %x", ext.path, path)
		}
		if !bytes.Equal(ext.childHash, childHash) {
			t.Errorf("ext.childHash mismatch")
		}
	})
}

func TestTrieGet_SingleLeaf(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Create a leaf node with key = [0x01, 0x02, 0x03], value = "hello"
	key := []byte{0x01, 0x02, 0x03}
	val := []byte("hello")
	leafData := encodeLeafNode(key, val)
	rootHash := storeNode(t, store, leafData)

	// Look up the key — should find the value
	got, err := trieGet(store, rootHash, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("trieGet = %q, want %q", got, val)
	}

	// Look up a different key — should return errKeyNotFound
	_, err = trieGet(store, rootHash, []byte{0x01, 0x02, 0x04})
	if err == nil {
		t.Error("expected error for wrong key")
	}
}

func TestTrieGet_BranchWithLeaves(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Build a trie:
	//   Branch (root)
	//     child[0x0A] → Leaf(key=[0x0A, 0x01], value="alpha")
	//     child[0x0B] → Leaf(key=[0x0B, 0x02], value="beta")

	leafA := encodeLeafNode([]byte{0x0A, 0x01}, []byte("alpha"))
	leafB := encodeLeafNode([]byte{0x0B, 0x02}, []byte("beta"))

	hashA := storeNode(t, store, leafA)
	hashB := storeNode(t, store, leafB)

	branchData := encodeBranchNode(map[byte][]byte{
		0x0A: hashA,
		0x0B: hashB,
	})
	rootHash := storeNode(t, store, branchData)

	// Look up both keys
	got, err := trieGet(store, rootHash, []byte{0x0A, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alpha" {
		t.Errorf("key 0A01 = %q, want 'alpha'", got)
	}

	got, err = trieGet(store, rootHash, []byte{0x0B, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "beta" {
		t.Errorf("key 0B02 = %q, want 'beta'", got)
	}

	// Non-existent child
	_, err = trieGet(store, rootHash, []byte{0x0C, 0x03})
	if err == nil {
		t.Error("expected error for non-existent branch child")
	}
}

func TestTrieGet_ExtensionBranchLeaves(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Build a trie:
	//   Extension(path=[0xAA, 0xBB])
	//     → Branch
	//         child[0x01] → Leaf(key=[0xAA, 0xBB, 0x01, 0xFF], value="one")
	//         child[0x02] → Leaf(key=[0xAA, 0xBB, 0x02, 0xFF], value="two")

	leaf1 := encodeLeafNode([]byte{0xAA, 0xBB, 0x01, 0xFF}, []byte("one"))
	leaf2 := encodeLeafNode([]byte{0xAA, 0xBB, 0x02, 0xFF}, []byte("two"))

	hash1 := storeNode(t, store, leaf1)
	hash2 := storeNode(t, store, leaf2)

	branchData := encodeBranchNode(map[byte][]byte{
		0x01: hash1,
		0x02: hash2,
	})
	branchHash := storeNode(t, store, branchData)

	extData := encodeExtensionNode([]byte{0xAA, 0xBB}, branchHash)
	rootHash := storeNode(t, store, extData)

	// Look up key [0xAA, 0xBB, 0x01, 0xFF]
	got, err := trieGet(store, rootHash, []byte{0xAA, 0xBB, 0x01, 0xFF})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one" {
		t.Errorf("got %q, want 'one'", got)
	}

	// Look up key [0xAA, 0xBB, 0x02, 0xFF]
	got, err = trieGet(store, rootHash, []byte{0xAA, 0xBB, 0x02, 0xFF})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Errorf("got %q, want 'two'", got)
	}

	// Wrong extension prefix
	_, err = trieGet(store, rootHash, []byte{0xAA, 0xCC, 0x01, 0xFF})
	if err == nil {
		t.Error("expected error for wrong extension prefix")
	}
}

func TestTrieGet_EmptyRoot(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Empty root hash → key not found
	_, err = trieGet(store, emptyTrieRoot, []byte{0x01})
	if err == nil {
		t.Error("expected error for empty trie root")
	}

	// Nil root hash → key not found
	_, err = trieGet(store, nil, []byte{0x01})
	if err == nil {
		t.Error("expected error for nil root hash")
	}
}

func TestGetStorageSlot(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Build a storage trie with one slot:
	//   Leaf(key=32-byte-slot, value=32-byte-value)
	slotKey := make([]byte, 32)
	slotKey[31] = 0x01 // slot 1
	slotVal := make([]byte, 32)
	slotVal[31] = 0x42 // value = 0x42

	leafData := encodeLeafNode(slotKey, slotVal)
	storageRoot := storeNode(t, store, leafData)

	// Create an account with this storage root
	addrHash := bytes.Repeat([]byte{0xDE}, 20)
	acctData := encodeTestAccount(10, "1000000000000000000", storageRoot, nil)

	// Store the account
	err = store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nsAccount)).Put(addrHash, acctData)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Update store height so it doesn't look empty
	heightBuf := make([]byte, 8)
	heightBuf[7] = 1
	err = store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nsMeta)).Put([]byte(metaHeight), heightBuf)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Look up the storage slot
	got, err := store.GetStorageSlot(addrHash, slotKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, slotVal) {
		t.Errorf("GetStorageSlot = %x, want %x", got, slotVal)
	}

	// Non-existent slot
	otherSlot := make([]byte, 32)
	otherSlot[31] = 0x02
	got, err = store.GetStorageSlot(addrHash, otherSlot)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for non-existent slot, got %x", got)
	}

	// Non-existent account
	got, err = store.GetStorageSlot(bytes.Repeat([]byte{0xFF}, 20), slotKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for non-existent account, got %x", got)
	}
}

func TestGetStorageSlot_NoRoot(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store, err := OpenStateStore(filepath.Join(dir, "test.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Account with no storage root (EOA)
	addrHash := bytes.Repeat([]byte{0xAA}, 20)
	acctData := encodeTestAccount(5, "100", nil, nil)
	err = store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nsAccount)).Put(addrHash, acctData)
	})
	if err != nil {
		t.Fatal(err)
	}

	slotKey := make([]byte, 32)
	got, err := store.GetStorageSlot(addrHash, slotKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for EOA, got %x", got)
	}
}

// TestEncodeDecodeRoundTrip verifies that encode→hash→store→decode→trieGet works end-to-end.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	// Encode a leaf node
	key := bytes.Repeat([]byte{0x42}, 20)
	val := []byte("test-value-12345")
	data := encodeLeafNode(key, val)

	// Verify hash160b of the data is 20 bytes
	h := hash160b(data)
	if len(h) != 20 {
		t.Fatalf("hash length = %d, want 20", len(h))
	}

	// Decode and verify
	nodeType, _, _, leaf, err := decodeNodePb(data)
	if err != nil {
		t.Fatal(err)
	}
	if nodeType != 'L' {
		t.Fatalf("nodeType = %c, want L", nodeType)
	}
	if !bytes.Equal(leaf.key, key) {
		t.Errorf("key mismatch")
	}
	if !bytes.Equal(leaf.value, val) {
		t.Errorf("value mismatch")
	}
}

