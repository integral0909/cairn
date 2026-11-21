// Package btree implements B+trees over pager pages. Keys are byte strings
// ordered bytewise (see value.EncodeKey); values are byte strings of any
// size, with large ones spilling into chains of overflow pages.
//
// A tree is identified by its root page, which never moves: when the root
// splits, its contents move to a new page and the root becomes the parent.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/integral0909/cairn/internal/pager"
)

// Reader reads pages (a pager.ReadTx or WriteTx).
type Reader interface {
	Page(pgno uint32) ([]byte, error)
}

// Writer also changes pages (a pager.WriteTx).
type Writer interface {
	Reader
	Modify(pgno uint32) ([]byte, error)
	Allocate() (uint32, []byte, error)
	Free(pgno uint32) error
}

const (
	pageSize   = pager.PageSize
	headerSize = 9
	typeLeaf   = 1
	typeInner  = 2
	// MaxKey is the longest key a tree accepts. Keys stay in the pages, so
	// they are bounded; values are not.
	MaxKey = 512
	// A cell never takes more than this much of a page, so every page holds
	// at least four cells and splits always work.
	maxCell      = (pageSize - headerSize) / 4
	overflowData = pageSize - 4
)

var (
	ErrKeyTooLarge = fmt.Errorf("btree: key longer than %d bytes", MaxKey)
	errCorrupt     = errors.New("btree: corrupt page")
)

// Page layout:
//
//	0     type (1 leaf, 2 interior)
//	1-2   number of cells
//	3-4   unused
//	5-8   rightmost child (interior pages)
//	9...  cells, packed
//
// Leaf cell:     uvarint key length, uvarint value length, key, the value
//                bytes stored here, and a 4-byte overflow page number when
//                the value did not fit.
// Interior cell: 4-byte child, uvarint key length, key. Keys in the child
//                are less than the cell's key; the rightmost child holds
//                keys greater than or equal to the last key.

type node struct {
	leaf     bool
	keys     [][]byte
	vals     [][]byte // leaf: the locally stored part of each value
	vlen     []int    // leaf: full value length
	ovf      []uint32 // leaf: first overflow page, or 0
	children []uint32 // interior: len(keys)+1
}

func localSize(klen, vlen int) int {
	over := uvarintLen(uint64(klen)) + uvarintLen(uint64(vlen))
	if over+klen+vlen <= maxCell {
		return vlen
	}
	return maxCell - over - klen - 4
}

func uvarintLen(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}

func (n *node) cellSize(i int) int {
	k := len(n.keys[i])
	if !n.leaf {
		return 4 + uvarintLen(uint64(k)) + k
	}
	s := uvarintLen(uint64(k)) + uvarintLen(uint64(n.vlen[i])) + k + len(n.vals[i])
	if n.ovf[i] != 0 {
		s += 4
	}
	return s
}

func (n *node) size() int {
	s := headerSize
	for i := range n.keys {
		s += n.cellSize(i)
	}
	return s
}

func decode(b []byte) (*node, error) {
	if len(b) != pageSize || (b[0] != typeLeaf && b[0] != typeInner) {
		return nil, errCorrupt
	}
	n := &node{leaf: b[0] == typeLeaf}
	count := int(binary.BigEndian.Uint16(b[1:]))
	p := headerSize
	read := func(want int) ([]byte, bool) {
		if want < 0 || p+want > len(b) {
			return nil, false
		}
		out := b[p : p+want]
		p += want
		return out, true
	}
	uv := func() (int, bool) {
		x, k := binary.Uvarint(b[p:])
		if k <= 0 || x > pageSize*1<<20 {
			return 0, false
		}
		p += k
		return int(x), true
	}
	n.keys = make([][]byte, count)
	if n.leaf {
		n.vals = make([][]byte, count)
		n.vlen = make([]int, count)
		n.ovf = make([]uint32, count)
	} else {
		n.children = make([]uint32, count+1)
	}
	for i := 0; i < count; i++ {
		if !n.leaf {
			c, ok := read(4)
			if !ok {
				return nil, errCorrupt
			}
			n.children[i] = binary.BigEndian.Uint32(c)
		}
		klen, ok := uv()
		if !ok {
			return nil, errCorrupt
		}
		if n.leaf {
			vlen, ok := uv()
			if !ok {
				return nil, errCorrupt
			}
			key, ok := read(klen)
			if !ok {
				return nil, errCorrupt
			}
			local := localSize(klen, vlen)
			val, ok := read(local)
			if !ok {
				return nil, errCorrupt
			}
			n.keys[i], n.vals[i], n.vlen[i] = key, val, vlen
			if local < vlen {
				o, ok := read(4)
				if !ok {
					return nil, errCorrupt
				}
				n.ovf[i] = binary.BigEndian.Uint32(o)
			}
		} else {
			key, ok := read(klen)
			if !ok {
				return nil, errCorrupt
			}
			n.keys[i] = key
		}
	}
	if !n.leaf {
		n.children[count] = binary.BigEndian.Uint32(b[5:])
	}
	return n, nil
}

// encode writes the node into b, which must be a whole page.
func (n *node) encode(b []byte) {
	clear(b)
	if n.leaf {
		b[0] = typeLeaf
	} else {
		b[0] = typeInner
		binary.BigEndian.PutUint32(b[5:], n.children[len(n.keys)])
	}
	binary.BigEndian.PutUint16(b[1:], uint16(len(n.keys)))
	p := headerSize
	for i, k := range n.keys {
		if !n.leaf {
			binary.BigEndian.PutUint32(b[p:], n.children[i])
			p += 4
		}
		p += binary.PutUvarint(b[p:], uint64(len(k)))
		if n.leaf {
			p += binary.PutUvarint(b[p:], uint64(n.vlen[i]))
		}
		p += copy(b[p:], k)
		if n.leaf {
			p += copy(b[p:], n.vals[i])
			if n.ovf[i] != 0 {
				binary.BigEndian.PutUint32(b[p:], n.ovf[i])
				p += 4
			}
		}
	}
}

// search returns the index of the first key >= k, and whether it is equal.
func (n *node) search(k []byte) (int, bool) {
	i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], k) >= 0 })
	return i, i < len(n.keys) && bytes.Equal(n.keys[i], k)
}

// child returns the index of the child that may hold k.
func (n *node) child(k []byte) int {
	return sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(k, n.keys[i]) < 0 })
}

func load(r Reader, pgno uint32) (*node, error) {
	b, err := r.Page(pgno)
	if err != nil {
		return nil, err
	}
	n, err := decode(b)
	if err != nil {
		return nil, fmt.Errorf("%w %d", err, pgno)
	}
	// Decoded slices alias the page; copy so later page changes cannot
	// disturb them.
	for i := range n.keys {
		n.keys[i] = append([]byte(nil), n.keys[i]...)
		if n.leaf {
			n.vals[i] = append([]byte(nil), n.vals[i]...)
		}
	}
	return n, nil
}

func store(w Writer, pgno uint32, n *node) error {
	b, err := w.Modify(pgno)
	if err != nil {
		return err
	}
	n.encode(b)
	return nil
}

// Create makes an empty tree and returns its root page.
func Create(w Writer) (uint32, error) {
	pgno, b, err := w.Allocate()
	if err != nil {
		return 0, err
	}
	(&node{leaf: true}).encode(b)
	return pgno, nil
}

// readValue assembles a value from its local part and overflow chain.
func readValue(r Reader, local []byte, total int, ovf uint32) ([]byte, error) {
	out := make([]byte, 0, total)
	out = append(out, local...)
	for ovf != 0 && len(out) < total {
		b, err := r.Page(ovf)
		if err != nil {
			return nil, err
		}
		take := min(total-len(out), overflowData)
		out = append(out, b[4:4+take]...)
		ovf = binary.BigEndian.Uint32(b)
	}
	if len(out) != total {
		return nil, errCorrupt
	}
	return out, nil
}

func writeOverflow(w Writer, data []byte) (uint32, error) {
	var first uint32
	var prevBuf []byte
	for len(data) > 0 {
		pg, b, err := w.Allocate()
		if err != nil {
			return 0, err
		}
		n := copy(b[4:], data)
		data = data[n:]
		if prevBuf != nil {
			binary.BigEndian.PutUint32(prevBuf, pg)
		} else {
			first = pg
		}
		prevBuf = b
	}
	return first, nil
}

func freeOverflow(w Writer, ovf uint32) error {
	for ovf != 0 {
		b, err := w.Page(ovf)
		if err != nil {
			return err
		}
		next := binary.BigEndian.Uint32(b)
		if err := w.Free(ovf); err != nil {
			return err
		}
		ovf = next
	}
	return nil
}

// Get looks up a key.
func Get(r Reader, root uint32, key []byte) ([]byte, bool, error) {
	pgno := root
	for {
		n, err := load(r, pgno)
		if err != nil {
			return nil, false, err
		}
		if !n.leaf {
			pgno = n.children[n.child(key)]
			continue
		}
		i, ok := n.search(key)
		if !ok {
			return nil, false, nil
		}
		v, err := readValue(r, n.vals[i], n.vlen[i], n.ovf[i])
		return v, err == nil, err
	}
}

// Put inserts a key or replaces its value.
func Put(w Writer, root uint32, key, val []byte) error {
	if len(key) > MaxKey {
		return ErrKeyTooLarge
	}
	sep, right, err := put(w, root, key, val)
	if err != nil || right == 0 {
		return err
	}
	// The root split: move its contents to a new page and make the root the
	// parent of that page and the new sibling.
	left, b, err := w.Allocate()
	if err != nil {
		return err
	}
	rb, err := w.Page(root)
	if err != nil {
		return err
	}
	copy(b, rb)
	return store(w, root, &node{keys: [][]byte{sep}, children: []uint32{left, right}})
}

// put returns a separator and new right sibling if the page split.
func put(w Writer, pgno uint32, key, val []byte) ([]byte, uint32, error) {
	n, err := load(w, pgno)
	if err != nil {
		return nil, 0, err
	}
	if n.leaf {
		i, found := n.search(key)
		local := localSize(len(key), len(val))
		var ovf uint32
		if local < len(val) {
			if ovf, err = writeOverflow(w, val[local:]); err != nil {
				return nil, 0, err
			}
		}
		if found {
			if err := freeOverflow(w, n.ovf[i]); err != nil {
				return nil, 0, err
			}
			n.vals[i], n.vlen[i], n.ovf[i] = val[:local], len(val), ovf
		} else {
			n.keys = insertAt(n.keys, i, key)
			n.vals = insertAt(n.vals, i, val[:local])
			n.vlen = insertAt(n.vlen, i, len(val))
			n.ovf = insertAt(n.ovf, i, ovf)
		}
	} else {
		i := n.child(key)
		sep, right, err := put(w, n.children[i], key, val)
		if err != nil {
			return nil, 0, err
		}
		if right == 0 {
			return nil, 0, nil
		}
		n.keys = insertAt(n.keys, i, sep)
		n.children = insertAt(n.children, i+1, right)
	}
	if n.size() <= pageSize {
		return nil, 0, store(w, pgno, n)
	}
	left, right, sep := split(n)
	rpg, _, err := w.Allocate()
	if err != nil {
		return nil, 0, err
	}
	if err := store(w, pgno, left); err != nil {
		return nil, 0, err
	}
	return sep, rpg, store(w, rpg, right)
}

// split divides an overfull node roughly in half by size.
func split(n *node) (left, right *node, sep []byte) {
	half := n.size() / 2
	acc := headerSize
	m := 0
	for m < len(n.keys)-1 && acc+n.cellSize(m) < half {
		acc += n.cellSize(m)
		m++
	}
	if m == 0 {
		m = 1
	}
	if n.leaf {
		left = &node{leaf: true, keys: n.keys[:m:m], vals: n.vals[:m:m], vlen: n.vlen[:m:m], ovf: n.ovf[:m:m]}
		right = &node{leaf: true, keys: n.keys[m:], vals: n.vals[m:], vlen: n.vlen[m:], ovf: n.ovf[m:]}
		return left, right, n.keys[m]
	}
	// The middle key moves up instead of staying in either half.
	left = &node{keys: n.keys[:m:m], children: n.children[: m+1 : m+1]}
	right = &node{keys: n.keys[m+1:], children: n.children[m+1:]}
	return left, right, n.keys[m]
}

func insertAt[T any](s []T, i int, v T) []T {
	s = append(s, v)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAt[T any](s []T, i int) []T {
	return append(s[:i], s[i+1:]...)
}

// Delete removes a key, reporting whether it was there.
func Delete(w Writer, root uint32, key []byte) (bool, error) {
	found, _, err := del(w, root, key)
	if err != nil || !found {
		return found, err
	}
	// A root left with a single child hands its contents up.
	for {
		n, err := load(w, root)
		if err != nil {
			return true, err
		}
		if n.leaf || len(n.keys) > 0 {
			return true, nil
		}
		only := n.children[0]
		cb, err := w.Page(only)
		if err != nil {
			return true, err
		}
		rb, err := w.Modify(root)
		if err != nil {
			return true, err
		}
		copy(rb, cb)
		if err := w.Free(only); err != nil {
			return true, err
		}
	}
}

const minFill = pageSize / 4

// del returns whether the key was found and whether the page is now
// underfull.
func del(w Writer, pgno uint32, key []byte) (bool, bool, error) {
	n, err := load(w, pgno)
	if err != nil {
		return false, false, err
	}
	if n.leaf {
		i, found := n.search(key)
		if !found {
			return false, false, nil
		}
		if err := freeOverflow(w, n.ovf[i]); err != nil {
			return false, false, err
		}
		n.keys = removeAt(n.keys, i)
		n.vals = removeAt(n.vals, i)
		n.vlen = removeAt(n.vlen, i)
		n.ovf = removeAt(n.ovf, i)
		return true, n.size() < minFill, store(w, pgno, n)
	}
	i := n.child(key)
	found, under, err := del(w, n.children[i], key)
	if err != nil || !found || !under {
		return found, false, err
	}
	// Try to merge the underfull child with a neighbour.
	l, r := i, i+1
	if r >= len(n.children) {
		l, r = i-1, i
	}
	if l < 0 {
		return true, n.size() < minFill, nil
	}
	left, err := load(w, n.children[l])
	if err != nil {
		return true, false, err
	}
	right, err := load(w, n.children[r])
	if err != nil {
		return true, false, err
	}
	merged := merge(left, right, n.keys[l])
	if merged.size() > pageSize {
		return true, false, nil
	}
	if err := store(w, n.children[l], merged); err != nil {
		return true, false, err
	}
	if err := w.Free(n.children[r]); err != nil {
		return true, false, err
	}
	n.keys = removeAt(n.keys, l)
	n.children = removeAt(n.children, r)
	return true, n.size() < minFill, store(w, pgno, n)
}

func merge(left, right *node, sep []byte) *node {
	if left.leaf {
		return &node{
			leaf: true,
			keys: append(left.keys, right.keys...),
			vals: append(left.vals, right.vals...),
			vlen: append(left.vlen, right.vlen...),
			ovf:  append(left.ovf, right.ovf...),
		}
	}
	keys := append(append(left.keys, sep), right.keys...)
	return &node{keys: keys, children: append(left.children, right.children...)}
}

// Drop frees every page of a tree, including its root.
func Drop(w Writer, root uint32) error {
	n, err := load(w, root)
	if err != nil {
		return err
	}
	if n.leaf {
		for _, o := range n.ovf {
			if err := freeOverflow(w, o); err != nil {
				return err
			}
		}
	} else {
		for _, c := range n.children {
			if err := Drop(w, c); err != nil {
				return err
			}
		}
	}
	return w.Free(root)
}

// Truncate removes every entry but keeps the root page.
func Truncate(w Writer, root uint32) error {
	n, err := load(w, root)
	if err != nil {
		return err
	}
	if n.leaf {
		for _, o := range n.ovf {
			if err := freeOverflow(w, o); err != nil {
				return err
			}
		}
	} else {
		for _, c := range n.children {
			if err := Drop(w, c); err != nil {
				return err
			}
		}
	}
	return store(w, root, &node{leaf: true})
}
