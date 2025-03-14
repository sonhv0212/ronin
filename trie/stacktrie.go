// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	stPool = sync.Pool{New: func() any { return new(stNode) }}
	_      = types.TrieHasher((*StackTrie)(nil)) // Ensure StackTrie implements the TrieHasher interface
)

// StackTrieOptions contains the configured options for manipulating the stackTrie.
type StackTrieOptions struct {
	Writer  func(path []byte, hash common.Hash, blob []byte) // The function to commit the dirty nodes
	Cleaner func(path []byte)                                // The function to clean up dangling nodes

	SkipLeftBoundary  bool          // Flag whether the nodes on the left boundary are skipped for committing
	SkipRightBoundary bool          // Flag whether the nodes on the right boundary are skipped for committing
	boundaryGauge     metrics.Gauge // Gauge to track how many boundary nodes are met
}

// NewStackTrieOptions initializes an empty options for stackTrie.
func NewStackTrieOptions() *StackTrieOptions { return &StackTrieOptions{} }

// WithWriter configures trie node writer within the options.
func (o *StackTrieOptions) WithWriter(writer func(path []byte, hash common.Hash, blob []byte)) *StackTrieOptions {
	o.Writer = writer
	return o
}

// WithCleaner configures the cleaner in the option for removing dangling nodes.
func (o *StackTrieOptions) WithCleaner(cleaner func(path []byte)) *StackTrieOptions {
	o.Cleaner = cleaner
	return o
}

// WithSkipBoundary configures whether the left and right boundary nodes are
// filtered for committing, along with a gauge metrics to track how many
// boundary nodes are met.
func (o *StackTrieOptions) WithSkipBoundary(skipLeft, skipRight bool, gauge metrics.Gauge) *StackTrieOptions {
	o.SkipLeftBoundary = skipLeft
	o.SkipRightBoundary = skipRight
	o.boundaryGauge = gauge
	return o
}

// StackTrie is a trie implementation that expects keys to be inserted
// in order. Once it determines that a subtree will no longer be inserted
// into, it will hash it and free up the memory it uses.
type StackTrie struct {
	options *StackTrieOptions
	root    *stNode
	h       *hasher

	first []byte // The (hex-encoded without terminator) key of first inserted entry, tracked as left boundary.
	last  []byte // The (hex-encoded without terminator) key of last inserted entry, tracked as right boundary.
}

// NewStackTrie allocates and initializes an empty trie.
func NewStackTrie(options *StackTrieOptions) *StackTrie {
	if options == nil {
		options = NewStackTrieOptions()
	}
	return &StackTrie{
		options: options,
		root:    stPool.Get().(*stNode),
		h:       newHasher(false),
	}
}

func (t *StackTrie) Update(key, value []byte) {
	if err := t.TryUpdate(key, value); err != nil {
		log.Error(fmt.Sprintf("Unhandled trie error: %v", err))
	}
}

// Update inserts a (key, value) pair into the stack trie.
func (t *StackTrie) TryUpdate(key, value []byte) error {
	k := keybytesToHex(key)
	if len(value) == 0 {
		panic("deletion not supported")
	}
	k = k[:len(k)-1] // chop the termination flag

	// track the first and last inserted entries.
	if t.first == nil {
		t.first = append([]byte{}, k...)
	}
	if t.last == nil {
		t.last = append([]byte{}, k...) // allocate key slice
	} else {
		t.last = append(t.last[:0], k...) // reuse key slice
	}
	t.insert(t.root, k, value, nil)
	return nil
}

// Reset resets the stack trie object to empty state.
func (t *StackTrie) Reset() {
	t.options = NewStackTrieOptions()
	t.root = stPool.Get().(*stNode)
	t.first = nil
	t.last = nil
}

// stNode represents a node within a StackTrie
type stNode struct {
	typ      uint8       // node type (as in branch, ext, leaf)
	key      []byte      // key chunk covered by this (leaf|ext) node
	val      []byte      // value contained by this node if it's a leaf
	children [16]*stNode // list of children (for branch and exts)
}

// newLeaf constructs a leaf node with provided node key and value. The key
// will be deep-copied in the function and safe to modify afterwards, but
// value is not.
func newLeaf(key, val []byte) *stNode {
	st := stPool.Get().(*stNode)
	st.typ = leafNode
	st.key = append(st.key, key...)
	st.val = val
	return st
}

// newExt constructs an extension node with provided node key and child. The
// key will be deep-copied in the function and safe to modify afterwards.
func newExt(key []byte, child *stNode) *stNode {
	st := stPool.Get().(*stNode)
	st.typ = extNode
	st.key = append(st.key, key...)
	st.children[0] = child
	return st
}

// List all values that stNode#nodeType can hold
const (
	emptyNode = iota
	branchNode
	extNode
	leafNode
	hashedNode
)

func (n *stNode) reset() *stNode {
	n.key = n.key[:0]
	n.val = nil
	for i := range n.children {
		n.children[i] = nil
	}
	n.typ = emptyNode
	return n
}

// Helper function that, given a full key, determines the index
// at which the chunk pointed by st.keyOffset is different from
// the same chunk in the full key.
func (n *stNode) getDiffIndex(key []byte) int {
	for idx, nibble := range n.key {
		if nibble != key[idx] {
			return idx
		}
	}
	return len(n.key)
}

// Helper function to that inserts a (key, value) pair into
// the trie.
func (t *StackTrie) insert(st *stNode, key, value []byte, path []byte) {
	switch st.typ {
	case branchNode: /* Branch */
		idx := int(key[0])
		// Unresolve elder siblings
		for i := idx - 1; i >= 0; i-- {
			if st.children[i] != nil {
				if st.children[i].typ != hashedNode {
					t.hash(st.children[i], append(path, byte(i)))
				}
				break
			}
		}
		// Add new child
		if st.children[idx] == nil {
			st.children[idx] = newLeaf(key[1:], value)
		} else {
			t.insert(st.children[idx], key[1:], value, append(path, key[0]))
		}

	case extNode: /* Ext */
		// Compare both key chunks and see where they differ
		diffidx := st.getDiffIndex(key)

		// Check if chunks are identical. If so, recurse into
		// the child node. Otherwise, the key has to be split
		// into 1) an optional common prefix, 2) the fullnode
		// representing the two differing path, and 3) a leaf
		// for each of the differentiated subtrees.
		if diffidx == len(st.key) {
			// Ext key and key segment are identical, recurse into
			// the child node.
			t.insert(st.children[0], key[diffidx:], value, append(path, key[:diffidx]...))
			return
		}
		// Save the original part. Depending if the break is
		// at the extension's last byte or not, create an
		// intermediate extension or use the extension's child
		// node directly.
		var n *stNode
		if diffidx < len(st.key)-1 {
			// Break on the non-last byte, insert an intermediate
			// extension. The path prefix of the newly-inserted
			// extension should also contain the different byte.
			n = newExt(st.key[diffidx+1:], st.children[0])
			t.hash(n, append(path, st.key[:diffidx+1]...))
		} else {
			// an extension node: reuse the current node.
			// The path prefix of the original part should
			// still be same.
			n = st.children[0]
			t.hash(n, append(path, st.key...))
		}
		var p *stNode
		if diffidx == 0 {
			// the break is on the first byte, so
			// the current node is converted into
			// a branch node.
			st.children[0] = nil
			p = st
			st.typ = branchNode
		} else {
			// the common prefix is at least one byte
			// long, insert a new intermediate branch
			// node.
			st.children[0] = stPool.Get().(*stNode)
			st.children[0].typ = branchNode
			p = st.children[0]
		}
		// Create a leaf for the inserted part
		o := newLeaf(key[diffidx+1:], value)

		// Insert both child leaves where they belong:
		origIdx := st.key[diffidx]
		newIdx := key[diffidx]
		p.children[origIdx] = n
		p.children[newIdx] = o
		st.key = st.key[:diffidx]

	case leafNode: /* Leaf */
		// Compare both key chunks and see where they differ
		diffidx := st.getDiffIndex(key)

		// Overwriting a key isn't supported, which means that
		// the current leaf is expected to be split into 1) an
		// optional extension for the common prefix of these 2
		// keys, 2) a fullnode selecting the path on which the
		// keys differ, and 3) one leaf for the differentiated
		// component of each key.
		if diffidx >= len(st.key) {
			panic("Trying to insert into existing key")
		}

		// Check if the split occurs at the first nibble of the
		// chunk. In that case, no prefix extnode is necessary.
		// Otherwise, create that
		var p *stNode
		if diffidx == 0 {
			// Convert current leaf into a branch
			st.typ = branchNode
			p = st
			st.children[0] = nil
		} else {
			// Convert current node into an ext,
			// and insert a child branch node.
			st.typ = extNode
			st.children[0] = stPool.Get().(*stNode)
			st.children[0].typ = branchNode
			p = st.children[0]
		}

		// Create the two child leaves: the one containing the
		// original value and the one containing the new value
		// The child leave will be hashed directly in order to
		// free up some memory.
		origIdx := st.key[diffidx]
		p.children[origIdx] = newLeaf(st.key[diffidx+1:], st.val)
		t.hash(p.children[origIdx], append(path, st.key[:diffidx+1]...))

		newIdx := key[diffidx]
		p.children[newIdx] = newLeaf(key[diffidx+1:], value)

		// Finally, cut off the key part that has been passed
		// over to the children.
		st.key = st.key[:diffidx]
		st.val = nil
	case emptyNode: /* Empty */
		st.typ = leafNode
		st.key = key
		st.val = value
	case hashedNode:
		panic("trying to insert into hash")
	default:
		panic("invalid type")
	}
}

// hash() hashes the node 'st' and converts it into 'hashedNode', if possible.
// Possible outcomes:
// 1. The rlp-encoded value was >= 32 bytes:
//   - Then the 32-byte `hash` will be accessible in `st.val`.
//   - And the 'st.type' will be 'hashedNode'
//
// 2. The rlp-encoded value was < 32 bytes
//   - Then the <32 byte rlp-encoded value will be accessible in 'st.val'.
//   - And the 'st.type' will be 'hashedNode' AGAIN
//
// This method will also:
// set 'st.type' to hashedNode
// clear 'st.key'
func (t *StackTrie) hash(st *stNode, path []byte) {
	/* Shortcut if node is already hashed */
	if st.typ == hashedNode {
		return
	}
	// The 'hasher' is taken from a pool, but we don't actually
	// claim an instance until all children are done with their hashing,
	// and we actually need one

	var (
		blob     []byte   // RLP-encoded node blob
		internal [][]byte // List of node paths covered by the extension node
	)

	switch st.typ {
	case branchNode:
		var node fullNode
		for i, child := range st.children {
			if child == nil {
				node.Children[i] = nilValueNode
				continue
			}
			t.hash(child, append(path, byte(i)))
			if len(child.val) < 32 {
				node.Children[i] = rawNode(child.val)
			} else {
				node.Children[i] = hashNode(child.val)
			}
			st.children[i] = nil      // Reclaim mem from subtree
			stPool.Put(child.reset()) // Release child back to pool.
		}

		node.encode(t.h.encbuf)
		blob = t.h.encodedBytes()
	case extNode:
		// recursively hash and commit child as the first step
		t.hash(st.children[0], append(path, st.key...))

		// Collect the path of internal nodes between shortNode and its **in disk**
		// child. This is essential in the case of path mode scheme to avoid leaving
		// danging nodes within the range of this internal path on disk, which would
		// break the guarantee for state healing.
		if len(st.children[0].val) >= 32 && t.options.Cleaner != nil {
			for i := 1; i < len(st.key); i++ {
				internal = append(internal, append(path, st.key[:i]...))
			}
		}
		// encode the extension node
		sz := hexToCompactInPlace(st.key)
		n := shortNode{Key: st.key[:sz]}

		if len(st.children[0].val) < 32 {
			n.Val = rawNode(st.children[0].val)
		} else {
			n.Val = hashNode(st.children[0].val)
		}

		n.encode(t.h.encbuf)
		blob = t.h.encodedBytes()

		stPool.Put(st.children[0].reset()) // Release child back to pool.
		st.children[0] = nil               // Reclaim mem from subtree
	case leafNode:

		st.key = append(st.key, byte(16))
		sz := hexToCompactInPlace(st.key)
		n := shortNode{Key: st.key[:sz], Val: valueNode(st.val)}

		n.encode(t.h.encbuf)
		blob = t.h.encodedBytes()
	case emptyNode:
		st.val = emptyRoot.Bytes()
		st.key = st.key[:0]
		st.typ = hashedNode
		return
	default:
		panic("Invalid node type")
	}
	st.key = st.key[:0]
	st.typ = hashedNode
	// Skip committing the non-root node if the size is smaller than 32 bytes.
	if len(blob) < 32 && len(path) > 0 {
		// If rlp-encoded value was < 32 bytes, then val point directly to the rlp-encoded value
		st.val = common.CopyBytes(blob)
		return
	}

	st.val = t.h.hashData(blob)

	// Short circuit if the stack trie is not configured for writing.
	if t.options.Writer == nil {
		return
	}
	// Skip committing if the node is on the left boundary and stackTrie is
	// configured to filter the boundary.
	if t.options.SkipLeftBoundary && bytes.HasPrefix(t.first, path) {
		if t.options.boundaryGauge != nil {
			t.options.boundaryGauge.Inc(1)
		}
		return
	}
	// Skip committing if the node is on the right boundary and stackTrie is
	// configured to filter the boundary.
	if t.options.SkipRightBoundary && bytes.HasPrefix(t.last, path) {
		if t.options.boundaryGauge != nil {
			t.options.boundaryGauge.Inc(1)
		}
		return
	}
	// Clean up the internal dangling nodes covered by the extension node.
	// This should be done before writing the node to adhere to the committing
	// order from bottom to top.
	for _, path := range internal {
		t.options.Cleaner(path)
	}
	t.options.Writer(path, common.BytesToHash(st.val), blob)
}

// Hash will firstly hash the entire trie if it's still not hashed and then commit
// all nodes to the associated database. Actually most of the trie nodes have been
// committed already. The main purpose here is to commit the nodes on right boundary.
//
// For stack trie, Hash and Commit are functionally identical
func (t *StackTrie) Hash() (h common.Hash) {
	n := t.root
	t.hash(n, nil)
	return common.BytesToHash(n.val)
}

// Commit will firstly hash the entire trie if it's still not hashed and then commit
// all nodes to the associated database. Actually most of the trie nodes have been
// committed already. The main purpose here is to commit the nodes on right boundary.
//
// For stack trie, Hash and Commit are functionally identical.
func (t *StackTrie) Commit() common.Hash {
	return t.Hash()
}
