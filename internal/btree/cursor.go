package btree

import (
	"bytes"
	"fmt"
)

// Cursor walks a tree in key order, forwards or backwards. It reads
// through the Reader it was opened with and must not be used while the
// same transaction changes the tree.
type Cursor struct {
	r     Reader
	root  uint32
	stack []frame
	err   error
}

type frame struct {
	n   *node
	idx int
}

// NewCursor opens a cursor; call Seek, First or Last to position it.
func NewCursor(r Reader, root uint32) *Cursor {
	return &Cursor{r: r, root: root}
}

func (c *Cursor) Err() error { return c.err }

// Valid reports whether the cursor is on an entry.
func (c *Cursor) Valid() bool {
	if c.err != nil || len(c.stack) == 0 {
		return false
	}
	top := c.stack[len(c.stack)-1]
	return top.n.leaf && top.idx >= 0 && top.idx < len(top.n.keys)
}

func (c *Cursor) top() *frame { return &c.stack[len(c.stack)-1] }

// Key returns the current key. Only valid until the next move.
func (c *Cursor) Key() []byte {
	t := c.top()
	return t.n.keys[t.idx]
}

// Value returns the current value, reading overflow pages if needed.
func (c *Cursor) Value() ([]byte, error) {
	t := c.top()
	if t.n.ovf[t.idx] == 0 {
		return t.n.vals[t.idx], nil
	}
	return readValue(c.r, t.n.vals[t.idx], t.n.vlen[t.idx], t.n.ovf[t.idx])
}

func (c *Cursor) push(pgno uint32) (*node, bool) {
	if len(c.stack) > 64 {
		c.err = fmt.Errorf("btree: tree deeper than 64 levels (corrupt)")
		return nil, false
	}
	n, err := load(c.r, pgno)
	if err != nil {
		c.err = err
		return nil, false
	}
	c.stack = append(c.stack, frame{n: n})
	return n, true
}

// Seek moves to the first entry with key >= k.
func (c *Cursor) Seek(k []byte) {
	c.stack = c.stack[:0]
	pgno := c.root
	for {
		n, ok := c.push(pgno)
		if !ok {
			return
		}
		if n.leaf {
			c.top().idx, _ = n.search(k)
			if c.top().idx >= len(n.keys) {
				c.top().idx = len(n.keys) - 1
				c.Next()
			}
			return
		}
		i := n.child(k)
		c.top().idx = i
		pgno = n.children[i]
	}
}

// First moves to the smallest key.
func (c *Cursor) First() {
	c.stack = c.stack[:0]
	c.descend(c.root, false)
}

// Last moves to the largest key.
func (c *Cursor) Last() {
	c.stack = c.stack[:0]
	c.descend(c.root, true)
}

// descend goes to the leftmost (or rightmost) leaf under pgno. Empty leaves
// (possible only for an empty tree) leave the cursor invalid.
func (c *Cursor) descend(pgno uint32, last bool) {
	for {
		n, ok := c.push(pgno)
		if !ok {
			return
		}
		if n.leaf {
			if last {
				c.top().idx = len(n.keys) - 1
			}
			return
		}
		i := 0
		if last {
			i = len(n.children) - 1
		}
		c.top().idx = i
		pgno = n.children[i]
	}
}

// Next moves to the following entry; the cursor becomes invalid past the
// end.
func (c *Cursor) Next() {
	if c.err != nil || len(c.stack) == 0 {
		return
	}
	c.top().idx++
	for c.top().idx >= len(c.top().n.keys) {
		// Leaf exhausted: climb to the first ancestor with a child to the
		// right, then go down its left edge.
		for {
			c.stack = c.stack[:len(c.stack)-1]
			if len(c.stack) == 0 {
				return
			}
			t := c.top()
			if t.idx+1 < len(t.n.children) {
				t.idx++
				c.descend(t.n.children[t.idx], false)
				break
			}
		}
		if c.err != nil {
			return
		}
	}
}

// Prev moves to the preceding entry.
func (c *Cursor) Prev() {
	if c.err != nil || len(c.stack) == 0 {
		return
	}
	c.top().idx--
	for c.top().idx < 0 {
		for {
			c.stack = c.stack[:len(c.stack)-1]
			if len(c.stack) == 0 {
				return
			}
			t := c.top()
			if t.idx > 0 {
				t.idx--
				c.descend(t.n.children[t.idx], true)
				break
			}
		}
		if c.err != nil {
			return
		}
	}
}

// Stats summarises a tree.
type Stats struct {
	Depth, Leaves, Interior, Overflow, Entries int
}

// Check verifies a tree's structure: key order within and across pages,
// separator bounds, equal leaf depth and readable overflow chains. It
// returns the pages the tree uses.
func Check(r Reader, root uint32) (Stats, []uint32, error) {
	var st Stats
	var pages []uint32
	leafDepth := -1
	var walk func(pgno uint32, lo, hi []byte, depth int) error
	walk = func(pgno uint32, lo, hi []byte, depth int) error {
		pages = append(pages, pgno)
		n, err := load(r, pgno)
		if err != nil {
			return err
		}
		for i, k := range n.keys {
			if i > 0 && bytes.Compare(n.keys[i-1], k) >= 0 {
				return fmt.Errorf("page %d: keys out of order", pgno)
			}
			if lo != nil && bytes.Compare(k, lo) < 0 {
				return fmt.Errorf("page %d: key below its lower bound", pgno)
			}
			if hi != nil && bytes.Compare(k, hi) >= 0 {
				return fmt.Errorf("page %d: key at or above its upper bound", pgno)
			}
		}
		if n.leaf {
			st.Leaves++
			st.Entries += len(n.keys)
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				return fmt.Errorf("page %d: leaves at different depths", pgno)
			}
			for i := range n.keys {
				for o := n.ovf[i]; o != 0; {
					pages = append(pages, o)
					st.Overflow++
					b, err := r.Page(o)
					if err != nil {
						return err
					}
					o = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
				}
				if _, err := readValue(r, n.vals[i], n.vlen[i], n.ovf[i]); err != nil {
					return fmt.Errorf("page %d: value %d: %w", pgno, i, err)
				}
			}
			return nil
		}
		st.Interior++
		if len(n.children) != len(n.keys)+1 {
			return fmt.Errorf("page %d: child count mismatch", pgno)
		}
		for i, child := range n.children {
			clo, chi := lo, hi
			if i > 0 {
				clo = n.keys[i-1]
			}
			if i < len(n.keys) {
				chi = n.keys[i]
			}
			if err := walk(child, clo, chi, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(root, nil, nil, 1)
	st.Depth = leafDepth
	return st, pages, err
}
