package main

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/crypto/ripemd160"
	"google.golang.org/protobuf/encoding/protowire"
)

// MPT trie node deserialization and traversal for ioswarm-agent.
//
// Reads trie nodes from the Contract namespace in BoltDB and traverses them
// to look up storage slot values. This enables fully independent L4 validation
// without relying on the coordinator's storage prefetch.
//
// Node format matches iotex-core's db/trie/mptrie (protobuf-based):
//   - Branch: up to 256 children indexed by byte value
//   - Extension: path prefix + single child hash
//   - Leaf: full key + value
//
// Hash function: Hash160b (RIPEMD-160, 20 bytes).

var (
	errNodeNotFound = errors.New("trie node not found")
	errKeyNotFound  = errors.New("key not found in trie")
	errInvalidNode  = errors.New("invalid trie node")
)

// emptyTrieRoot is the Hash160b of an empty trie root (from iotex-core tests).
var emptyTrieRoot = []byte{
	0x61, 0x8e, 0x1c, 0xe1, 0xff, 0xfb, 0x18, 0x25,
	0x15, 0x9f, 0x9a, 0xa2, 0xc2, 0xe9, 0x37, 0x24,
	0x02, 0xfb, 0xd0, 0xab,
}

// hash160b computes RIPEMD-160 of data, matching iotex-core's hash.Hash160b.
func hash160b(data []byte) []byte {
	h := ripemd160.New()
	h.Write(data)
	return h.Sum(nil)
}

// trieGet traverses an MPT trie starting from rootHash to find the value
// for the given key. Reads trie nodes from the StateStore's Contract namespace.
//
// The traversal is iterative (not recursive) to avoid stack depth issues on
// deeply nested tries.
func trieGet(store *StateStore, rootHash []byte, key []byte) ([]byte, error) {
	if len(rootHash) == 0 || bytes.Equal(rootHash, emptyTrieRoot) {
		return nil, errKeyNotFound
	}

	nodeHash := rootHash
	offset := 0

	for {
		// Load serialized node from Contract namespace
		data, err := store.Get(nsContract, nodeHash)
		if err != nil {
			return nil, fmt.Errorf("load trie node %x: %w", nodeHash, err)
		}
		if data == nil {
			return nil, fmt.Errorf("trie node %x: %w", nodeHash, errNodeNotFound)
		}

		// Decode the NodePb protobuf
		nodeType, branch, ext, leaf, err := decodeNodePb(data)
		if err != nil {
			return nil, fmt.Errorf("decode trie node %x: %w", nodeHash, err)
		}

		switch nodeType {
		case 'B': // branch
			if offset >= len(key) {
				return nil, errKeyNotFound
			}
			childHash, ok := branch[key[offset]]
			if !ok {
				return nil, errKeyNotFound
			}
			nodeHash = childHash
			offset++

		case 'E': // extension
			if offset+len(ext.path) > len(key) {
				return nil, errKeyNotFound
			}
			if !bytes.Equal(ext.path, key[offset:offset+len(ext.path)]) {
				return nil, errKeyNotFound
			}
			nodeHash = ext.childHash
			offset += len(ext.path)

		case 'L': // leaf
			if offset > len(leaf.key) || offset > len(key) {
				return nil, errKeyNotFound
			}
			if !bytes.Equal(leaf.key[offset:], key[offset:]) {
				return nil, errKeyNotFound
			}
			return leaf.value, nil

		default:
			return nil, errInvalidNode
		}
	}
}

// --- Protobuf decoding (manual, using protowire) ---

// mptExtNode holds decoded extension node data.
type mptExtNode struct {
	path      []byte
	childHash []byte
}

// mptLeafNode holds decoded leaf node data.
type mptLeafNode struct {
	key   []byte
	value []byte
}

// decodeNodePb decodes a protobuf-serialized NodePb (iotex-core triepb.NodePb).
// Returns: nodeType ('B'ranch/'E'xtension/'L'eaf), branch children, extension, leaf.
//
// NodePb { oneof node { branchPb branch = 2; leafPb leaf = 3; extendPb extend = 4; } }
func decodeNodePb(data []byte) (byte, map[byte][]byte, *mptExtNode, *mptLeafNode, error) {
	buf := data
	for len(buf) > 0 {
		num, wtype, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return 0, nil, nil, nil, errInvalidNode
		}
		buf = buf[n:]

		if wtype == protowire.BytesType {
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return 0, nil, nil, nil, errInvalidNode
			}
			buf = buf[n:]

			switch num {
			case 2: // branchPb
				children, err := decodeBranchPb(v)
				if err != nil {
					return 0, nil, nil, nil, err
				}
				return 'B', children, nil, nil, nil

			case 3: // leafPb
				leaf, err := decodeLeafPb(v)
				if err != nil {
					return 0, nil, nil, nil, err
				}
				return 'L', nil, nil, leaf, nil

			case 4: // extendPb
				ext, err := decodeExtendPb(v)
				if err != nil {
					return 0, nil, nil, nil, err
				}
				return 'E', nil, ext, nil, nil
			}
		} else {
			n, err := skipField(buf, wtype)
			if err != nil {
				return 0, nil, nil, nil, err
			}
			buf = buf[n:]
		}
	}
	return 0, nil, nil, nil, errInvalidNode
}

// decodeBranchPb decodes a branchPb message.
// branchPb { repeated branchNodePb branches = 1; }
func decodeBranchPb(data []byte) (map[byte][]byte, error) {
	children := make(map[byte][]byte)
	buf := data
	for len(buf) > 0 {
		num, wtype, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return nil, errInvalidNode
		}
		buf = buf[n:]

		if num == 1 && wtype == protowire.BytesType {
			// embedded branchNodePb
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]

			idx, path, err := decodeBranchNodePb(v)
			if err != nil {
				return nil, err
			}
			children[byte(idx)] = path
		} else {
			n, err := skipField(buf, wtype)
			if err != nil {
				return nil, err
			}
			buf = buf[n:]
		}
	}
	return children, nil
}

// decodeBranchNodePb decodes a branchNodePb message.
// branchNodePb { uint32 index = 1; bytes path = 2; }
func decodeBranchNodePb(data []byte) (uint32, []byte, error) {
	var index uint32
	var path []byte
	buf := data

	for len(buf) > 0 {
		num, wtype, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return 0, nil, errInvalidNode
		}
		buf = buf[n:]

		switch {
		case num == 1 && wtype == protowire.VarintType:
			v, n := protowire.ConsumeVarint(buf)
			if n < 0 {
				return 0, nil, errInvalidNode
			}
			buf = buf[n:]
			index = uint32(v)

		case num == 2 && wtype == protowire.BytesType:
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return 0, nil, errInvalidNode
			}
			buf = buf[n:]
			path = make([]byte, len(v))
			copy(path, v)

		default:
			n, err := skipField(buf, wtype)
			if err != nil {
				return 0, nil, err
			}
			buf = buf[n:]
		}
	}
	return index, path, nil
}

// decodeLeafPb decodes a leafPb message.
// leafPb { uint32 ext = 1; bytes path = 2; bytes value = 3; }
func decodeLeafPb(data []byte) (*mptLeafNode, error) {
	leaf := &mptLeafNode{}
	buf := data

	for len(buf) > 0 {
		num, wtype, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return nil, errInvalidNode
		}
		buf = buf[n:]

		switch {
		case num == 1 && wtype == protowire.VarintType:
			// ext field — unused/legacy, skip
			_, n := protowire.ConsumeVarint(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]

		case num == 2 && wtype == protowire.BytesType:
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]
			leaf.key = make([]byte, len(v))
			copy(leaf.key, v)

		case num == 3 && wtype == protowire.BytesType:
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]
			leaf.value = make([]byte, len(v))
			copy(leaf.value, v)

		default:
			n, err := skipField(buf, wtype)
			if err != nil {
				return nil, err
			}
			buf = buf[n:]
		}
	}
	return leaf, nil
}

// decodeExtendPb decodes an extendPb message.
// extendPb { bytes path = 1; bytes value = 2; }
func decodeExtendPb(data []byte) (*mptExtNode, error) {
	ext := &mptExtNode{}
	buf := data

	for len(buf) > 0 {
		num, wtype, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return nil, errInvalidNode
		}
		buf = buf[n:]

		switch {
		case num == 1 && wtype == protowire.BytesType:
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]
			ext.path = make([]byte, len(v))
			copy(ext.path, v)

		case num == 2 && wtype == protowire.BytesType:
			v, n := protowire.ConsumeBytes(buf)
			if n < 0 {
				return nil, errInvalidNode
			}
			buf = buf[n:]
			ext.childHash = make([]byte, len(v))
			copy(ext.childHash, v)

		default:
			n, err := skipField(buf, wtype)
			if err != nil {
				return nil, err
			}
			buf = buf[n:]
		}
	}
	return ext, nil
}

// skipField skips a protobuf field value based on wire type.
func skipField(buf []byte, wtype protowire.Type) (int, error) {
	switch wtype {
	case protowire.VarintType:
		_, n := protowire.ConsumeVarint(buf)
		if n < 0 {
			return 0, errInvalidNode
		}
		return n, nil
	case protowire.BytesType:
		_, n := protowire.ConsumeBytes(buf)
		if n < 0 {
			return 0, errInvalidNode
		}
		return n, nil
	case protowire.Fixed32Type:
		_, n := protowire.ConsumeFixed32(buf)
		if n < 0 {
			return 0, errInvalidNode
		}
		return n, nil
	case protowire.Fixed64Type:
		_, n := protowire.ConsumeFixed64(buf)
		if n < 0 {
			return 0, errInvalidNode
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unsupported wire type %d", wtype)
	}
}

// --- Protobuf encoding helpers (for building trie nodes in tests and tools) ---

// encodeLeafNode builds a serialized NodePb containing a leafPb.
func encodeLeafNode(key, value []byte) []byte {
	// Build leafPb: path(2,bytes) + value(3,bytes)
	var leaf []byte
	leaf = protowire.AppendTag(leaf, 2, protowire.BytesType)
	leaf = protowire.AppendBytes(leaf, key)
	leaf = protowire.AppendTag(leaf, 3, protowire.BytesType)
	leaf = protowire.AppendBytes(leaf, value)

	// Wrap in NodePb: leaf(3,bytes)
	var node []byte
	node = protowire.AppendTag(node, 3, protowire.BytesType)
	node = protowire.AppendBytes(node, leaf)
	return node
}

// encodeBranchNode builds a serialized NodePb containing a branchPb.
// children is a map of byte index → child node hash.
func encodeBranchNode(children map[byte][]byte) []byte {
	// Build branchPb: repeated branchNodePb(1,bytes)
	var branch []byte
	for idx, hash := range children {
		// Build branchNodePb: index(1,varint) + path(2,bytes)
		var bnp []byte
		bnp = protowire.AppendTag(bnp, 1, protowire.VarintType)
		bnp = protowire.AppendVarint(bnp, uint64(idx))
		bnp = protowire.AppendTag(bnp, 2, protowire.BytesType)
		bnp = protowire.AppendBytes(bnp, hash)

		branch = protowire.AppendTag(branch, 1, protowire.BytesType)
		branch = protowire.AppendBytes(branch, bnp)
	}

	// Wrap in NodePb: branch(2,bytes)
	var node []byte
	node = protowire.AppendTag(node, 2, protowire.BytesType)
	node = protowire.AppendBytes(node, branch)
	return node
}

// encodeExtensionNode builds a serialized NodePb containing an extendPb.
func encodeExtensionNode(path, childHash []byte) []byte {
	// Build extendPb: path(1,bytes) + value(2,bytes)
	var ext []byte
	ext = protowire.AppendTag(ext, 1, protowire.BytesType)
	ext = protowire.AppendBytes(ext, path)
	ext = protowire.AppendTag(ext, 2, protowire.BytesType)
	ext = protowire.AppendBytes(ext, childHash)

	// Wrap in NodePb: extend(4,bytes)
	var node []byte
	node = protowire.AppendTag(node, 4, protowire.BytesType)
	node = protowire.AppendBytes(node, ext)
	return node
}
