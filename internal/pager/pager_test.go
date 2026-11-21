package pager

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"testing"
)

func openMem(t *testing.T, fs *MemFS, opts Options) *Pager {
	t.Helper()
	p, err := Open(fs.Open("db"), fs.Open("db-wal"), opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stamp writes a recognisable value into a page.
func stamp(t *testing.T, tx *WriteTx, pgno uint32, v uint64) {
	t.Helper()
	b, err := tx.Modify(pgno)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(b[100:], v)
	binary.BigEndian.PutUint64(b[PageSize-8:], v)
}

func readStamp(t *testing.T, page func(uint32) ([]byte, error), pgno uint32) uint64 {
	t.Helper()
	b, err := page(pgno)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[100:108], b[PageSize-8:]) {
		t.Fatalf("page %d is torn", pgno)
	}
	return binary.BigEndian.Uint64(b[100:])
}

func TestWriteReadCommitRollback(t *testing.T) {
	p := openMem(t, NewMemFS(), Options{})
	tx := p.BeginWrite()
	pg, _, err := tx.Allocate()
	if err != nil || pg != 1 {
		t.Fatalf("first allocation = %d, %v", pg, err)
	}
	stamp(t, tx, pg, 7)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = p.BeginWrite()
	stamp(t, tx, pg, 8)
	tx.Rollback()
	tx2 := p.BeginWrite()
	if _, err := tx2.Modify(99); err == nil {
		t.Fatal("modifying a page past the end must fail")
	}
	tx2.Rollback()
	r := p.BeginRead()
	if got := readStamp(t, r.Page, pg); got != 7 {
		t.Fatalf("after rollback got %d", got)
	}
	r.Close()
	if err := tx.Commit(); err != ErrTxDone {
		t.Fatalf("commit after rollback: %v", err)
	}
}

func TestReadersSeeASnapshot(t *testing.T) {
	p := openMem(t, NewMemFS(), Options{CheckpointFrames: 2})
	tx := p.BeginWrite()
	pg, _, _ := tx.Allocate()
	stamp(t, tx, pg, 1)
	tx.Commit()

	old := p.BeginRead()
	for v := uint64(2); v <= 10; v++ {
		tx := p.BeginWrite()
		stamp(t, tx, pg, v)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if got := readStamp(t, old.Page, pg); got != 1 {
		t.Fatalf("old reader saw %d, want 1", got)
	}
	fresh := p.BeginRead()
	if got := readStamp(t, fresh.Page, pg); got != 10 {
		t.Fatalf("new reader saw %d, want 10", got)
	}
	if p.Stats().LogFrames == 0 {
		t.Fatal("no checkpoint should run while readers are open")
	}
	old.Close()
	fresh.Close()
	if err := p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if p.Stats().LogFrames != 0 {
		t.Fatal("checkpoint should empty the log")
	}
	r := p.BeginRead()
	if got := readStamp(t, r.Page, pg); got != 10 {
		t.Fatalf("after checkpoint saw %d", got)
	}
	r.Close()
}

func TestFreeListReusesPages(t *testing.T) {
	p := openMem(t, NewMemFS(), Options{})
	tx := p.BeginWrite()
	a, _, _ := tx.Allocate()
	b, _, _ := tx.Allocate()
	c, _, _ := tx.Allocate()
	tx.Commit()
	tx = p.BeginWrite()
	tx.Free(b)
	tx.Free(a)
	if n, _ := tx.FreePages(); n != 2 {
		t.Fatalf("free pages = %d", n)
	}
	x, buf, _ := tx.Allocate()
	y, _, _ := tx.Allocate()
	z, _, _ := tx.Allocate()
	if x != a || y != b || z != c+1 {
		t.Fatalf("allocated %d %d %d; want %d %d %d", x, y, z, a, b, c+1)
	}
	if !bytes.Equal(buf, make([]byte, PageSize)) {
		t.Fatal("reused pages must come back zeroed")
	}
	tx.Commit()
	if p.Stats().Pages != 5 {
		t.Fatalf("pages = %d", p.Stats().Pages)
	}
}

func TestReopenFromDisk(t *testing.T) {
	dir := t.TempDir()
	open := func() *Pager {
		db, _ := OpenFile(filepath.Join(dir, "x.db"))
		w, _ := OpenFile(filepath.Join(dir, "x.db-wal"))
		p, err := Open(db, w, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := open()
	tx := p.BeginWrite()
	for i := 0; i < 50; i++ {
		pg, _, _ := tx.Allocate()
		stamp(t, tx, pg, uint64(i*i))
	}
	tx.Commit()
	p.Close()
	p = open()
	r := p.BeginRead()
	for i := 0; i < 50; i++ {
		if got := readStamp(t, r.Page, uint32(i+1)); got != uint64(i*i) {
			t.Fatalf("page %d = %d", i+1, got)
		}
	}
	r.Close()
	p.Close()
}

func TestRejectsOtherFiles(t *testing.T) {
	fs := NewMemFS()
	f := fs.Open("db")
	f.WriteAt(bytes.Repeat([]byte("not a database "), 400), 0)
	if _, err := Open(f, fs.Open("db-wal"), Options{}); err != ErrNotDatabase {
		t.Fatalf("got %v", err)
	}
}

// Random transactions, then a simulated power cut at a random moment. After
// reopening, the database must hold exactly the last committed state: no
// lost commits (every commit was synced), no partial ones, no torn pages.
func TestCrashRecovery(t *testing.T) {
	for seed := int64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		fs := NewMemFS()
		p := openMem(t, fs, Options{CheckpointFrames: 1 + r.Intn(40)})
		model := map[uint32]uint64{}
		var pages []uint32
		steps := 1 + r.Intn(30)
		for i := 0; i < steps; i++ {
			tx := p.BeginWrite()
			known := len(pages)
			next := map[uint32]uint64{}
			for k, v := range model {
				next[k] = v
			}
			for j := 0; j < 1+r.Intn(6); j++ {
				if len(pages) == 0 || r.Intn(3) == 0 {
					pg, _, err := tx.Allocate()
					if err != nil {
						t.Fatal(err)
					}
					pages = append(pages, pg)
				}
				pg := pages[r.Intn(len(pages))]
				v := r.Uint64()
				stamp(t, tx, pg, v)
				next[pg] = v
			}
			if r.Intn(5) == 0 {
				tx.Rollback()
				pages = pages[:known] // pages it allocated no longer exist
				continue
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			model = next
		}
		// The crash: some unsynced writes may land, the last possibly torn.
		crashed := fs.Crash(r)
		p2, err := Open(crashed.Open("db"), crashed.Open("db-wal"), Options{})
		if err != nil {
			t.Fatalf("seed %d: reopen: %v", seed, err)
		}
		rd := p2.BeginRead()
		for pg, want := range model {
			if got := readStamp(t, rd.Page, pg); got != want {
				t.Fatalf("seed %d: page %d = %d, want %d", seed, pg, got, want)
			}
		}
		rd.Close()
		// And it keeps working after recovery.
		tx := p2.BeginWrite()
		pg, _, err := tx.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		stamp(t, tx, pg, 42)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		p2.Close()
	}
}

// A commit that is interrupted before its sync must vanish entirely.
func TestUnsyncedCommitIsAllOrNothing(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		fs := NewMemFS()
		p := openMem(t, fs, Options{NoSync: true})
		tx := p.BeginWrite()
		var pages []uint32
		for i := 0; i < 8; i++ {
			pg, _, _ := tx.Allocate()
			pages = append(pages, pg)
			stamp(t, tx, pg, 1)
		}
		tx.Commit()
		p.Checkpoint() // syncs: state 1 is durable
		tx = p.BeginWrite()
		for _, pg := range pages {
			stamp(t, tx, pg, 2)
		}
		tx.Commit() // not synced
		crashed := fs.Crash(r)
		p2, err := Open(crashed.Open("db"), crashed.Open("db-wal"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		rd := p2.BeginRead()
		first := readStamp(t, rd.Page, pages[0])
		for _, pg := range pages {
			if got := readStamp(t, rd.Page, pg); got != first {
				t.Fatalf("seed %d: half a commit survived (page %d = %d, page %d = %d)", seed, pages[0], first, pg, got)
			}
		}
		rd.Close()
	}
}

func TestWriteErrorPoisonsPager(t *testing.T) {
	fs := NewMemFS()
	p := openMem(t, fs, Options{})
	fs.Open("db-wal").FailAfter = 1
	tx := p.BeginWrite()
	pg, _, _ := tx.Allocate()
	stamp(t, tx, pg, 1)
	if err := tx.Commit(); err == nil {
		t.Fatal("commit should fail")
	}
	tx = p.BeginWrite()
	if _, err := tx.Modify(1); err == nil {
		t.Fatal("a broken pager must refuse to read pages")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("a broken pager must refuse further commits")
	}
}

// Readers running alongside a writer always see a consistent state: every
// page carries the number of the commit that wrote it, and all pages were
// written together.
func TestConcurrentReadersAndWriter(t *testing.T) {
	p := openMem(t, NewMemFS(), Options{CheckpointFrames: 16})
	tx := p.BeginWrite()
	for i := 0; i < 4; i++ {
		pg, _, _ := tx.Allocate()
		stamp(t, tx, pg, 0)
	}
	tx.Commit()
	done := make(chan struct{})
	errs := make(chan string, 8)
	for w := 0; w < 4; w++ {
		go func() {
			for {
				select {
				case <-done:
					errs <- ""
					return
				default:
				}
				r := p.BeginRead()
				var first uint64
				for pg := uint32(1); pg <= 4; pg++ {
					b, err := r.Page(pg)
					if err != nil {
						errs <- err.Error()
						r.Close()
						return
					}
					v := binary.BigEndian.Uint64(b[100:])
					if pg == 1 {
						first = v
					} else if v != first {
						errs <- "reader saw pages from different commits"
						r.Close()
						return
					}
				}
				r.Close()
			}
		}()
	}
	for v := uint64(1); v <= 300; v++ {
		tx := p.BeginWrite()
		for pg := uint32(1); pg <= 4; pg++ {
			stamp(t, tx, pg, v)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	for i := 0; i < 4; i++ {
		if e := <-errs; e != "" {
			t.Fatal(e)
		}
	}
}

func TestStatementRollbackKeepsEarlierWork(t *testing.T) {
	p := openMem(t, NewMemFS(), Options{})
	tx := p.BeginWrite()
	a, _, _ := tx.Allocate()
	stamp(t, tx, a, 1)
	tx.BeginStatement()
	stamp(t, tx, a, 2)
	b, _, _ := tx.Allocate()
	stamp(t, tx, b, 3)
	tx.RollbackStatement()
	if got := readStamp(t, tx.Page, a); got != 1 {
		t.Fatalf("page a = %d after statement rollback", got)
	}
	if tx.PageCount() != 2 {
		t.Fatalf("the statement's allocation should be undone, %d pages", tx.PageCount())
	}
	tx.BeginStatement()
	stamp(t, tx, a, 4)
	tx.EndStatement()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := p.BeginRead()
	if got := readStamp(t, r.Page, a); got != 4 {
		t.Fatalf("committed %d", got)
	}
	r.Close()
}
