package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/integral0909/cairn/internal/pager"
)

func newPager(t *testing.T) *pager.Pager {
	t.Helper()
	fs := pager.NewMemFS()
	p, err := pager.Open(fs.Open("db"), fs.Open("wal"), pager.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func key(i int) []byte { return []byte(fmt.Sprintf("key%08d", i)) }

func TestPutGetDeleteSmall(t *testing.T) {
	p := newPager(t)
	tx := p.BeginWrite()
	root, err := Create(tx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := Put(tx, root, key(i), []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	v, ok, err := Get(tx, root, key(42))
	if !ok || err != nil || string(v) != "42" {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	Put(tx, root, key(42), []byte("forty-two"))
	v, _, _ = Get(tx, root, key(42))
	if string(v) != "forty-two" {
		t.Fatalf("replace failed: %q", v)
	}
	if ok, _ := Delete(tx, root, key(42)); !ok {
		t.Fatal("delete reported missing")
	}
	if ok, _ := Delete(tx, root, key(42)); ok {
		t.Fatal("second delete reported found")
	}
	if _, ok, _ := Get(tx, root, key(42)); ok {
		t.Fatal("deleted key still found")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLargeKeysAndValues(t *testing.T) {
	p := newPager(t)
	tx := p.BeginWrite()
	root, _ := Create(tx)
	if err := Put(tx, root, bytes.Repeat([]byte("k"), MaxKey+1), nil); err != ErrKeyTooLarge {
		t.Fatalf("oversized key: %v", err)
	}
	big := bytes.Repeat([]byte("0123456789"), 5000) // 50 KB, many overflow pages
	Put(tx, root, []byte("big"), big)
	got, ok, err := Get(tx, root, []byte("big"))
	if !ok || err != nil || !bytes.Equal(got, big) {
		t.Fatalf("big value round trip failed: %v %v %d", ok, err, len(got))
	}
	st, _, err := Check(tx, root)
	if err != nil || st.Overflow < 12 {
		t.Fatalf("check: %+v %v", st, err)
	}
	// Replacing and deleting give the overflow pages back.
	free0, _ := tx.FreePages()
	Put(tx, root, []byte("big"), []byte("small"))
	free1, _ := tx.FreePages()
	if free1-free0 != st.Overflow {
		t.Fatalf("replacing freed %d pages, want %d", free1-free0, st.Overflow)
	}
	tx.Commit()
}

// A random workload checked against a sorted map, with the tree's
// structure verified and every page accounted for (in the tree or on the
// free list) after each batch.
func TestRandomAgainstModel(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		r := rand.New(rand.NewSource(seed))
		p := newPager(t)
		tx := p.BeginWrite()
		root, _ := Create(tx)
		model := map[string][]byte{}
		space := 300 + r.Intn(3000)
		for step := 0; step < 4000; step++ {
			k := key(r.Intn(space))
			switch op := r.Intn(10); {
			case op < 6:
				v := make([]byte, r.Intn(40))
				if r.Intn(30) == 0 {
					v = make([]byte, 1000+r.Intn(9000)) // overflow
				}
				r.Read(v)
				if err := Put(tx, root, k, v); err != nil {
					t.Fatal(err)
				}
				model[string(k)] = v
			case op < 9:
				ok, err := Delete(tx, root, k)
				if err != nil {
					t.Fatal(err)
				}
				_, want := model[string(k)]
				if ok != want {
					t.Fatalf("seed %d: delete %s = %v, want %v", seed, k, ok, want)
				}
				delete(model, string(k))
			default:
				v, ok, err := Get(tx, root, k)
				if err != nil {
					t.Fatal(err)
				}
				want, wok := model[string(k)]
				if ok != wok || !bytes.Equal(v, want) {
					t.Fatalf("seed %d: get %s mismatch", seed, k)
				}
			}
			if step%500 == 499 {
				verify(t, tx, root, model)
			}
		}
		verify(t, tx, root, model)
		// Delete everything: the tree shrinks back to a single empty leaf.
		for k := range model {
			Delete(tx, root, []byte(k))
		}
		st, pages, err := Check(tx, root)
		if err != nil || st.Entries != 0 || len(pages) != 1 {
			t.Fatalf("seed %d: emptied tree: %+v, %d pages, %v", seed, st, len(pages), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func verify(t *testing.T, tx *pager.WriteTx, root uint32, model map[string][]byte) {
	t.Helper()
	st, pages, err := Check(tx, root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != len(model) {
		t.Fatalf("tree has %d entries, model %d", st.Entries, len(model))
	}
	free, err := tx.FreePages()
	if err != nil {
		t.Fatal(err)
	}
	// Page 0 is the header; everything else is in the tree or free.
	if int(tx.PageCount()) != 1+len(pages)+free {
		t.Fatalf("leaked pages: %d total, %d in tree, %d free", tx.PageCount(), len(pages), free)
	}
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := NewCursor(tx, root)
	i := 0
	for c.First(); c.Valid(); c.Next() {
		if string(c.Key()) != keys[i] {
			t.Fatalf("cursor at %d: %s, want %s", i, c.Key(), keys[i])
		}
		v, _ := c.Value()
		if !bytes.Equal(v, model[keys[i]]) {
			t.Fatalf("cursor value mismatch at %s", keys[i])
		}
		i++
	}
	if c.Err() != nil || i != len(keys) {
		t.Fatalf("forward scan saw %d of %d (%v)", i, len(keys), c.Err())
	}
	i = len(keys) - 1
	for c.Last(); c.Valid(); c.Prev() {
		if string(c.Key()) != keys[i] {
			t.Fatalf("backward scan mismatch at %d", i)
		}
		i--
	}
	if i != -1 {
		t.Fatalf("backward scan stopped early at %d", i)
	}
}

func TestSeek(t *testing.T) {
	p := newPager(t)
	tx := p.BeginWrite()
	root, _ := Create(tx)
	for i := 0; i < 2000; i += 2 {
		Put(tx, root, key(i), nil)
	}
	c := NewCursor(tx, root)
	c.Seek(key(501))
	if !c.Valid() || string(c.Key()) != string(key(502)) {
		t.Fatalf("seek 501 landed on %s", c.Key())
	}
	c.Seek(key(500))
	if string(c.Key()) != string(key(500)) {
		t.Fatal("seek to an existing key")
	}
	c.Prev()
	if string(c.Key()) != string(key(498)) {
		t.Fatal("prev after seek")
	}
	c.Seek(key(5000))
	if c.Valid() {
		t.Fatal("seek past the end should be invalid")
	}
	c.Seek(nil)
	if string(c.Key()) != string(key(0)) {
		t.Fatal("seek to nil finds the first key")
	}
	empty, _ := Create(tx)
	e := NewCursor(tx, empty)
	for _, move := range []func(){e.First, e.Last, func() { e.Seek(key(1)) }} {
		move()
		if e.Valid() {
			t.Fatal("an empty tree has no entries")
		}
	}
}

func TestDropAndTruncateFreeEverything(t *testing.T) {
	p := newPager(t)
	tx := p.BeginWrite()
	root, _ := Create(tx)
	for i := 0; i < 3000; i++ {
		Put(tx, root, key(i), bytes.Repeat([]byte{1}, i%1500))
	}
	if err := Truncate(tx, root); err != nil {
		t.Fatal(err)
	}
	free, _ := tx.FreePages()
	if int(tx.PageCount()) != 2+free {
		t.Fatalf("truncate kept pages: %d total, %d free", tx.PageCount(), free)
	}
	Put(tx, root, key(1), nil)
	if err := Drop(tx, root); err != nil {
		t.Fatal(err)
	}
	free, _ = tx.FreePages()
	if int(tx.PageCount()) != 1+free {
		t.Fatal("drop should free the root too")
	}
}

// Readers keep their snapshot of a tree while a writer rewrites it.
func TestSnapshotIsolation(t *testing.T) {
	p := newPager(t)
	tx := p.BeginWrite()
	root, _ := Create(tx)
	for i := 0; i < 1000; i++ {
		Put(tx, root, key(i), []byte("old"))
	}
	tx.Commit()
	r := p.BeginRead()
	tx = p.BeginWrite()
	for i := 0; i < 1000; i++ {
		if i%2 == 0 {
			Delete(tx, root, key(i))
		} else {
			Put(tx, root, key(i), []byte("new"))
		}
	}
	tx.Commit()
	n := 0
	c := NewCursor(r, root)
	for c.First(); c.Valid(); c.Next() {
		if v, _ := c.Value(); string(v) != "old" {
			t.Fatal("reader saw a newer value")
		}
		n++
	}
	if n != 1000 {
		t.Fatalf("reader saw %d entries", n)
	}
	r.Close()
}

func BenchmarkPutSequential(b *testing.B) {
	fs := pager.NewMemFS()
	p, _ := pager.Open(fs.Open("db"), fs.Open("wal"), pager.Options{NoSync: true})
	tx := p.BeginWrite()
	root, _ := Create(tx)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Put(tx, root, key(i), []byte("value"))
	}
}

func BenchmarkGetRandom(b *testing.B) {
	fs := pager.NewMemFS()
	p, _ := pager.Open(fs.Open("db"), fs.Open("wal"), pager.Options{NoSync: true})
	tx := p.BeginWrite()
	root, _ := Create(tx)
	for i := 0; i < 100000; i++ {
		Put(tx, root, key(i), []byte("value"))
	}
	r := rand.New(rand.NewSource(1))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Get(tx, root, key(r.Intn(100000)))
	}
}
