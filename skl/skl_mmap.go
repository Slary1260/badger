/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
Adapted from RocksDB inline skiplist.

Key differences:
- No optimization for sequential inserts (no "prev").
- No custom comparator.
- Support overwrites. This requires care when we see the same key when inserting.
  For RocksDB or LevelDB, overwrites are implemented as a newer sequence number in the key, so
	there is no need for values. We don't intend to support versioning. In-place updates of values
	would be more efficient.
- We discard all non-concurrent code.
- We do not support Splices. This simplifies the code a lot.
- No AllocateNode or other pointer arithmetic.
- We combine the findLessThan, findGreaterOrEqual, etc into one function.
*/

package skl

import (
	"bytes"
	"sync/atomic"

	"github.com/dgraph-io/badger/v2/y"
	"github.com/dgraph-io/ristretto/z"
)

// SkiplistMmap maps keys to values (in memory)
type SkiplistMmap struct {
	height int32 // Current height. 1 <= height <= kMaxHeight. CAS.
	head   *node
	ref    int32
	arena  *ArenaMmap
}

// IncrRef increases the refcount
func (s *SkiplistMmap) IncrRef() {
	atomic.AddInt32(&s.ref, 1)
}

// DecrRef decrements the refcount, deallocating the Skiplist when done using it
func (s *SkiplistMmap) DecrRef() {
	newRef := atomic.AddInt32(&s.ref, -1)
	if newRef > 0 {
		return
	}

	s.arena.reset()
	// Indicate we are closed. Good for testing.  Also, lets GC reclaim memory. Race condition
	// here would suggest we are accessing skiplist when we are supposed to have no reference!
	s.arena = nil
	// Since the head references the arena's buf, as long as the head is kept around
	// GC can't release the buf.
	s.head = nil
}

func newNodeMmap(arena *ArenaMmap, key string, uid uint64, height int) *node {
	// The base level is already allocated in the node struct.
	offset := arena.putNode(height)
	node := arena.getNode(offset)
	node.keyOffset = arena.putKey(key)
	node.keySize = uint16(len(key))
	node.height = uint16(height)
	// node.value = encodeValue(arena.putVal(v), v.EncodedSize())
	node.value = uid
	return node
}

func zeroOut(data []byte, offset int) {
	data = data[offset:]
	data[0] = 0x00
	for bp := 1; bp < len(data); bp *= 2 {
		copy(data[bp:], data[:bp])
	}
}

// NewSkiplist makes a new empty skiplist, with a given arena size
func NewSkiplistMmap(arenaSize int64) *SkiplistMmap {
	arena := newArenaMmap(arenaSize)
	head := newNodeMmap(arena, "", 0, maxHeight)
	return &SkiplistMmap{
		height: 1,
		head:   head,
		arena:  arena,
		ref:    1,
	}
}

func (s *node) keyMmap(arena *ArenaMmap) []byte {
	return arena.getKey(s.keyOffset, s.keySize)
}

func (s *node) getValue() uint64 {
	return atomic.LoadUint64(&s.value)
}

func (s *SkiplistMmap) randomHeight() int {
	h := 1
	for h < maxHeight && z.FastRand() <= heightIncrease {
		h++
	}
	return h
}

func (s *SkiplistMmap) getNext(nd *node, height int) *node {
	return s.arena.getNode(nd.getNextOffset(height))
}

// findNear finds the node near to key.
// If less=true, it finds rightmost node such that node.key < key (if allowEqual=false) or
// node.key <= key (if allowEqual=true).
// If less=false, it finds leftmost node such that node.key > key (if allowEqual=false) or
// node.key >= key (if allowEqual=true).
// Returns the node found. The bool returned is true if the node has key equal to given key.
func (s *SkiplistMmap) findNear(key string, less bool, allowEqual bool) (*node, bool) {
	x := s.head
	level := int(s.getHeight() - 1)
	for {
		// Assume x.key < key.
		next := s.getNext(x, level)
		if next == nil {
			// x.key < key < END OF LIST
			if level > 0 {
				// Can descend further to iterate closer to the end.
				level--
				continue
			}
			// Level=0. Cannot descend further. Let's return something that makes sense.
			if !less {
				return nil, false
			}
			// Try to return x. Make sure it is not a head node.
			if x == s.head {
				return nil, false
			}
			return x, false
		}

		nextKey := next.keyMmap(s.arena)
		cmp := bytes.Compare([]byte(key), nextKey)
		if cmp > 0 {
			// x.key < next.key < key. We can continue to move right.
			x = next
			continue
		}
		if cmp == 0 {
			// x.key < key == next.key.
			if allowEqual {
				return next, true
			}
			if !less {
				// We want >, so go to base level to grab the next bigger note.
				return s.getNext(next, 0), false
			}
			// We want <. If not base level, we should go closer in the next level.
			if level > 0 {
				level--
				continue
			}
			// On base level. Return x.
			if x == s.head {
				return nil, false
			}
			return x, false
		}
		// cmp < 0. In other words, x.key < key < next.
		if level > 0 {
			level--
			continue
		}
		// At base level. Need to return something.
		if !less {
			return next, false
		}
		// Try to return x. Make sure it is not a head node.
		if x == s.head {
			return nil, false
		}
		return x, false
	}
}

// findSpliceForLevel returns (outBefore, outAfter) with outBefore.key <= key <= outAfter.key.
// The input "before" tells us where to start looking.
// If we found a node with the same key, then we return outBefore = outAfter.
// Otherwise, outBefore.key < key < outAfter.key.
func (s *SkiplistMmap) findSpliceForLevel(key string, before *node, level int) (*node, *node) {
	for {
		// Assume before.key < key.
		next := s.getNext(before, level)
		if next == nil {
			return before, next
		}
		nextKey := next.keyMmap(s.arena)
		cmp := bytes.Compare([]byte(key), nextKey)
		if cmp == 0 {
			// Equality case.
			return next, next
		}
		if cmp < 0 {
			// before.key < key < next.key. We are done for this level.
			return before, next
		}
		before = next // Keep moving right on this level.
	}
}

func (s *SkiplistMmap) getHeight() int32 {
	return atomic.LoadInt32(&s.height)
}

// Put inserts the key-value pair.
func (s *SkiplistMmap) Put(key string, uid uint64) {
	// Since we allow overwrite, we may not need to create a new node. We might not even need to
	// increase the height. Let's defer these actions.

	listHeight := s.getHeight()
	var prev [maxHeight + 1]*node
	var next [maxHeight + 1]*node
	prev[listHeight] = s.head
	next[listHeight] = nil
	for i := int(listHeight) - 1; i >= 0; i-- {
		// Use higher level to speed up for current level.
		prev[i], next[i] = s.findSpliceForLevel(key, prev[i+1], i)
		if prev[i] == next[i] {
			// prev[i].setValue(s.arena, uid)
			prev[i].value = uid
			return
		}
	}

	// We do need to create a new node.
	height := s.randomHeight()
	x := newNodeMmap(s.arena, key, uid, height)

	// Try to increase s.height via CAS.
	listHeight = s.getHeight()
	for height > int(listHeight) {
		if atomic.CompareAndSwapInt32(&s.height, listHeight, int32(height)) {
			// Successfully increased skiplist.height.
			break
		}
		listHeight = s.getHeight()
	}

	// We always insert from the base level and up. After you add a node in base level, we cannot
	// create a node in the level above because it would have discovered the node in the base level.
	for i := 0; i < height; i++ {
		for {
			if prev[i] == nil {
				y.AssertTrue(i > 1) // This cannot happen in base level.
				// We haven't computed prev, next for this level because height exceeds old listHeight.
				// For these levels, we expect the lists to be sparse, so we can just search from head.
				prev[i], next[i] = s.findSpliceForLevel(key, s.head, i)
				// Someone adds the exact same key before we are able to do so. This can only happen on
				// the base level. But we know we are not on the base level.
				y.AssertTrue(prev[i] != next[i])
			}
			nextOffset := s.arena.getNodeOffset(next[i])
			x.tower[i] = nextOffset
			if prev[i].casNextOffset(i, nextOffset, s.arena.getNodeOffset(x)) {
				// Managed to insert x between prev[i] and next[i]. Go to the next level.
				break
			}
			// CAS failed. We need to recompute prev and next.
			// It is unlikely to be helpful to try to use a different level as we redo the search,
			// because it is unlikely that lots of nodes are inserted between prev[i] and next[i].
			prev[i], next[i] = s.findSpliceForLevel(key, prev[i], i)
			if prev[i] == next[i] {
				y.AssertTruef(i == 0, "Equality can happen only on base level: %d", i)
				prev[i].value = uid
				// prev[i].setValue(s.arena, uid)
				return
			}
		}
	}
}

// Empty returns if the Skiplist is empty.
func (s *SkiplistMmap) Empty() bool {
	return s.findLast() == nil
}

// findLast returns the last element. If head (empty list), we return nil. All the find functions
// will NEVER return the head nodes.
func (s *SkiplistMmap) findLast() *node {
	n := s.head
	level := int(s.getHeight()) - 1
	for {
		next := s.getNext(n, level)
		if next != nil {
			n = next
			continue
		}
		if level == 0 {
			if n == s.head {
				return nil
			}
			return n
		}
		level--
	}
}

// Get gets the value associated with the key. It returns a valid value if it finds equal or earlier
// version of the same key.
func (s *SkiplistMmap) Get(key string) uint64 {
	n, _ := s.findNear(key, false, true) // findGreaterOrEqual.
	if n == nil {
		return 0
	}

	nextKey := s.arena.getKey(n.keyOffset, n.keySize)
	if !bytes.Equal([]byte(key), nextKey) {
		return 0
	}

	return n.getValue()
	// valOffset, valSize := n.getValue()
	// vs := s.arena.getVal(valOffset, valSize)
	// vs.Version = y.ParseTs(nextKey)
	// return vs
}

// NewIterator returns a skiplist iterator.  You have to Close() the iterator.
func (s *SkiplistMmap) NewIterator() *IteratorMmap {
	s.IncrRef()
	return &IteratorMmap{list: s}
}

// MemSize returns the size of the Skiplist in terms of how much memory is used within its internal
// arena.
func (s *SkiplistMmap) MemSize() int64 { return s.arena.size() }

// IteratorMmap is an iterator over skiplist object. For new objects, you just
// need to initialize IteratorMmap.list.
type IteratorMmap struct {
	list *SkiplistMmap
	n    *node
}

// Close frees the resources held by the iterator
func (s *IteratorMmap) Close() error {
	s.list.DecrRef()
	return nil
}

// Valid returns true iff the iterator is positioned at a valid node.
func (s *IteratorMmap) Valid() bool { return s.n != nil }

// Key returns the key at the current position.
func (s *IteratorMmap) Key() string {
	return string(s.list.arena.getKey(s.n.keyOffset, s.n.keySize))
}

// Value returns value.
func (s *IteratorMmap) Value() uint64 {
	return s.n.getValue()
	// valOffset, valSize := s.n.getValue()
	// return s.list.arena.getVal(valOffset, valSize)
}

// Next advances to the next position.
func (s *IteratorMmap) Next() {
	y.AssertTrue(s.Valid())
	s.n = s.list.getNext(s.n, 0)
}

// Prev advances to the previous position.
func (s *IteratorMmap) Prev() {
	y.AssertTrue(s.Valid())
	s.n, _ = s.list.findNear(s.Key(), true, false) // find <. No equality allowed.
}

// Seek advances to the first entry with a key >= target.
func (s *IteratorMmap) Seek(target string) {
	s.n, _ = s.list.findNear(target, false, true) // find >=.
}

// SeekForPrev finds an entry with key <= target.
func (s *IteratorMmap) SeekForPrev(target string) {
	s.n, _ = s.list.findNear(target, true, true) // find <=.
}

// SeekToFirst seeks position at the first entry in list.
// Final state of iterator is Valid() iff list is not empty.
func (s *IteratorMmap) SeekToFirst() {
	s.n = s.list.getNext(s.list.head, 0)
}

// SeekToLast seeks position at the last entry in list.
// Final state of iterator is Valid() iff list is not empty.
func (s *IteratorMmap) SeekToLast() {
	s.n = s.list.findLast()
}

// UniIteratorMmap is a unidirectional memtable iterator. It is a thin wrapper around
// Iterator. We like to keep Iterator as before, because it is more powerful and
// we might support bidirectional iterators in the future.
type UniIteratorMmap struct {
	iter     *IteratorMmap
	reversed bool
}

// NewUniIterator returns a UniIterator.
func (s *SkiplistMmap) NewUniIterator(reversed bool) *UniIteratorMmap {
	return &UniIteratorMmap{
		iter:     s.NewIterator(),
		reversed: reversed,
	}
}

// Next implements y.Interface
func (s *UniIteratorMmap) Next() {
	if !s.reversed {
		s.iter.Next()
	} else {
		s.iter.Prev()
	}
}

// Rewind implements y.Interface
func (s *UniIteratorMmap) Rewind() {
	if !s.reversed {
		s.iter.SeekToFirst()
	} else {
		s.iter.SeekToLast()
	}
}

// Seek implements y.Interface
func (s *UniIteratorMmap) Seek(key string) {
	if !s.reversed {
		s.iter.Seek(key)
	} else {
		s.iter.SeekForPrev(key)
	}
}

// Key implements y.Interface
func (s *UniIteratorMmap) Key() []byte { return []byte(s.iter.Key()) }

// Value implements y.Interface
func (s *UniIteratorMmap) Value() uint64 { return s.iter.Value() }

// Valid implements y.Interface
func (s *UniIteratorMmap) Valid() bool { return s.iter.Valid() }

// Close implements y.Interface (and frees up the iter's resources)
func (s *UniIteratorMmap) Close() error { return s.iter.Close() }
