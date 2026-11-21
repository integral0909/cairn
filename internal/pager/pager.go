package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// PageSize is the size of every page.
const PageSize = 4096

// Page 0 starts with the database header. The pager owns the first 32
// bytes; bytes from UserHeader on belong to the layers above.
const (
	offMagic     = 0
	offPageSize  = 16
	offPageCount = 20
	offFreeHead  = 24
	UserHeader   = 64
)

var magic = []byte("cairn format 1\x00\x00")

var (
	ErrNotDatabase = errors.New("file is not a cairn database")
	ErrClosed      = errors.New("database is closed")
	ErrTxDone      = errors.New("transaction already finished")
)

// Options tune the pager.
type Options struct {
	// NoSync skips fsync on commit: faster, but a crash may lose the last
	// commits (never corrupt the file).
	NoSync bool
	// CheckpointFrames is how long the log may grow before its pages are
	// copied into the database file. Default 1000.
	CheckpointFrames int
	// CachePages bounds the page cache. Default 2048 (8 MiB).
	CachePages int
}

// Pager hands out read and write transactions over the pages of one
// database. Any number of readers can run alongside one writer: each reader
// sees the database as of the moment it began.
type Pager struct {
	db   File
	w    *wal
	opts Options

	mu      sync.Mutex // guards w's index, cache, readers, closed
	writeMu sync.Mutex // held by the one active write transaction
	cache   *cache
	readers map[*ReadTx]struct{}
	closed  bool
	broken  error
}

// Open opens a database file and its log, recovering committed
// transactions from the log and initialising an empty database.
func Open(db, walFile File, opts Options) (*Pager, error) {
	if opts.CheckpointFrames <= 0 {
		opts.CheckpointFrames = 1000
	}
	if opts.CachePages <= 0 {
		opts.CachePages = 2048
	}
	w, err := openWAL(walFile, PageSize)
	if err != nil {
		return nil, err
	}
	p := &Pager{db: db, w: w, opts: opts, cache: newCache(opts.CachePages), readers: map[*ReadTx]struct{}{}}
	size, err := db.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 && w.mxFrame == 0 {
		tx := p.BeginWrite()
		hdr := make([]byte, PageSize)
		tx.dirty[0] = hdr
		copy(hdr[offMagic:], magic)
		binary.BigEndian.PutUint32(hdr[offPageSize:], PageSize)
		binary.BigEndian.PutUint32(hdr[offPageCount:], 1)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return p, nil
	}
	r := p.BeginRead()
	defer r.Close()
	hdr, err := r.Page(0)
	if err != nil {
		return nil, err
	}
	if string(hdr[:len(magic)]) != string(magic) || binary.BigEndian.Uint32(hdr[offPageSize:]) != PageSize {
		return nil, ErrNotDatabase
	}
	return p, nil
}

// Close checkpoints the log into the database file and closes both.
func (p *Pager) Close() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var err error
	if p.broken == nil && len(p.readers) == 0 {
		err = p.checkpointLocked()
	}
	p.mu.Unlock()
	return errors.Join(err, p.w.f.Close(), p.db.Close())
}

// Checkpoint copies committed pages from the log into the database file and
// empties the log. It does nothing while readers are active.
func (p *Pager) Checkpoint() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if len(p.readers) > 0 {
		return nil
	}
	return p.checkpointLocked()
}

// checkpointLocked runs with writeMu and mu held and no readers. If it is
// interrupted, the log is still intact and recovery redoes the copy.
func (p *Pager) checkpointLocked() error {
	if p.w.mxFrame == 0 {
		return nil
	}
	buf := make([]byte, PageSize)
	pages := make([]uint32, 0, len(p.w.frameOf))
	for pgno := range p.w.frameOf {
		pages = append(pages, pgno)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	for _, pgno := range pages {
		if pgno >= p.w.dbSize {
			continue
		}
		f := p.w.find(pgno, p.w.mxFrame)
		if f < 0 {
			continue
		}
		if err := p.w.readFrame(f, buf); err != nil {
			return p.fail(err)
		}
		if _, err := p.db.WriteAt(buf, int64(pgno)*PageSize); err != nil {
			return p.fail(err)
		}
	}
	if err := p.db.Truncate(int64(p.w.dbSize) * PageSize); err != nil {
		return p.fail(err)
	}
	if err := p.db.Sync(); err != nil {
		return p.fail(err)
	}
	if err := p.w.reset(p.w.seq + 1); err != nil {
		return p.fail(err)
	}
	p.cache.clear()
	return nil
}

func (p *Pager) fail(err error) error {
	if p.broken == nil {
		p.broken = fmt.Errorf("pager: %w (reopen the database to recover)", err)
	}
	return p.broken
}

// Stats describes the pager's state.
type Stats struct {
	Pages      uint32 // pages in the database
	LogFrames  int    // committed frames waiting to be checkpointed
	CachePages int
	Readers    int
}

func (p *Pager) Stats() Stats {
	r := p.BeginRead()
	defer r.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Pages: r.dbSize, LogFrames: p.w.mxFrame, CachePages: len(p.cache.pages), Readers: len(p.readers) - 1}
}

// snapshot is a consistent view: committed frames below mxFrame, plus the
// database file.
type snapshot struct {
	mxFrame int
	dbSize  uint32
}

func (p *Pager) currentSnapshot() snapshot {
	if p.w.mxFrame > 0 {
		return snapshot{p.w.mxFrame, p.w.dbSize}
	}
	size, _ := p.db.Size()
	return snapshot{0, uint32(size / PageSize)}
}

// read returns the committed image of pgno as of snap. The slice is shared
// with the cache and must not be modified.
func (p *Pager) read(snap snapshot, pgno uint32) ([]byte, error) {
	if pgno >= snap.dbSize {
		return nil, fmt.Errorf("pager: page %d is past the end of the database (%d pages)", pgno, snap.dbSize)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken != nil {
		return nil, p.broken
	}
	frame := p.w.find(pgno, snap.mxFrame)
	key := cacheKey{pgno, frame}
	if b := p.cache.get(key); b != nil {
		return b, nil
	}
	buf := make([]byte, PageSize)
	if frame >= 0 {
		if err := p.w.readFrame(frame, buf); err != nil {
			return nil, err
		}
	} else if _, err := p.db.ReadAt(buf, int64(pgno)*PageSize); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	p.cache.put(key, buf)
	return buf, nil
}

// ---- read transactions -----------------------------------------------------

// ReadTx reads pages as of the moment it began.
type ReadTx struct {
	p      *Pager
	snap   snapshot
	dbSize uint32
	done   bool
}

func (p *Pager) BeginRead() *ReadTx {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.currentSnapshot()
	r := &ReadTx{p: p, snap: snap, dbSize: snap.dbSize}
	p.readers[r] = struct{}{}
	return r
}

// Page returns a page. The bytes must not be modified.
func (r *ReadTx) Page(pgno uint32) ([]byte, error) {
	if r.done {
		return nil, ErrTxDone
	}
	return r.p.read(r.snap, pgno)
}

func (r *ReadTx) Close() {
	if r.done {
		return
	}
	r.done = true
	r.p.mu.Lock()
	delete(r.p.readers, r)
	r.p.mu.Unlock()
}

// ---- write transactions ----------------------------------------------------

// WriteTx changes pages. Changes stay private until Commit; Rollback (or a
// failed Commit) discards them.
type WriteTx struct {
	p     *Pager
	snap  snapshot
	dirty map[uint32][]byte
	done  bool
	// stmt journals the pages a statement changes so that a failing
	// statement can be undone without ending the transaction. A nil entry
	// means the page was not dirty before the statement.
	stmt map[uint32][]byte
}

// BeginWrite waits for any other write transaction to finish.
func (p *Pager) BeginWrite() *WriteTx {
	p.writeMu.Lock()
	p.mu.Lock()
	snap := p.currentSnapshot()
	p.mu.Unlock()
	return &WriteTx{p: p, snap: snap, dirty: map[uint32][]byte{}}
}

// Page returns the current content of a page (including this
// transaction's changes). The bytes must not be modified; use Modify.
func (t *WriteTx) Page(pgno uint32) ([]byte, error) {
	if t.done {
		return nil, ErrTxDone
	}
	if b, ok := t.dirty[pgno]; ok {
		return b, nil
	}
	return t.p.read(t.snap, pgno)
}

// Modify returns a private, writable copy of a page.
func (t *WriteTx) Modify(pgno uint32) ([]byte, error) {
	if t.done {
		return nil, ErrTxDone
	}
	if b, ok := t.dirty[pgno]; ok {
		if t.stmt != nil {
			if _, seen := t.stmt[pgno]; !seen {
				t.stmt[pgno] = append([]byte(nil), b...)
			}
		}
		return b, nil
	}
	if pgno >= t.PageCount() {
		return nil, fmt.Errorf("pager: page %d is past the end of the database (%d pages)", pgno, t.PageCount())
	}
	var b []byte
	// Pages past the snapshot's end were allocated by this transaction and
	// start out zeroed.
	if pgno < t.snap.dbSize {
		src, err := t.p.read(t.snap, pgno)
		if err != nil {
			return nil, err
		}
		b = append(make([]byte, 0, PageSize), src...)
	} else {
		b = make([]byte, PageSize)
	}
	t.dirty[pgno] = b
	if t.stmt != nil {
		if _, seen := t.stmt[pgno]; !seen {
			t.stmt[pgno] = nil
		}
	}
	return b, nil
}

// BeginStatement starts journaling, so RollbackStatement can undo
// everything changed from here on.
func (t *WriteTx) BeginStatement() { t.stmt = map[uint32][]byte{} }

// EndStatement keeps the statement's changes.
func (t *WriteTx) EndStatement() { t.stmt = nil }

// RollbackStatement undoes the changes made since BeginStatement.
func (t *WriteTx) RollbackStatement() {
	for pgno, orig := range t.stmt {
		if orig == nil {
			delete(t.dirty, pgno)
		} else {
			t.dirty[pgno] = orig
		}
	}
	t.stmt = nil
}

// PageCount is the number of pages in the database, including pages this
// transaction allocated.
func (t *WriteTx) PageCount() uint32 {
	if h, ok := t.dirty[0]; ok {
		return binary.BigEndian.Uint32(h[offPageCount:])
	}
	return t.snap.dbSize
}

func (t *WriteTx) hdr() ([]byte, error) { return t.Modify(0) }

// Allocate returns a zeroed page, reusing a freed one if there is one.
func (t *WriteTx) Allocate() (uint32, []byte, error) {
	h, err := t.hdr()
	if err != nil {
		return 0, nil, err
	}
	if head := binary.BigEndian.Uint32(h[offFreeHead:]); head != 0 {
		b, err := t.Modify(head)
		if err != nil {
			return 0, nil, err
		}
		binary.BigEndian.PutUint32(h[offFreeHead:], binary.BigEndian.Uint32(b))
		clear(b)
		return head, b, nil
	}
	pgno := binary.BigEndian.Uint32(h[offPageCount:])
	binary.BigEndian.PutUint32(h[offPageCount:], pgno+1)
	b, err := t.Modify(pgno)
	return pgno, b, err
}

// Free puts a page on the free list.
func (t *WriteTx) Free(pgno uint32) error {
	if pgno == 0 {
		return errors.New("pager: cannot free the header page")
	}
	h, err := t.hdr()
	if err != nil {
		return err
	}
	b, err := t.Modify(pgno)
	if err != nil {
		return err
	}
	clear(b)
	binary.BigEndian.PutUint32(b, binary.BigEndian.Uint32(h[offFreeHead:]))
	binary.BigEndian.PutUint32(h[offFreeHead:], pgno)
	return nil
}

// FreePages counts the pages on the free list.
func (t *WriteTx) FreePages() (int, error) {
	h, err := t.Page(0)
	if err != nil {
		return 0, err
	}
	n := 0
	for pg := binary.BigEndian.Uint32(h[offFreeHead:]); pg != 0; n++ {
		b, err := t.Page(pg)
		if err != nil {
			return n, err
		}
		pg = binary.BigEndian.Uint32(b)
		if n > int(t.PageCount()) {
			return n, errors.New("pager: free list has a cycle")
		}
	}
	return n, nil
}

// Commit makes the changes durable and visible to new readers.
func (t *WriteTx) Commit() error {
	if t.done {
		return ErrTxDone
	}
	t.done = true
	defer t.p.writeMu.Unlock()
	p := t.p
	p.mu.Lock()
	broken, closed := p.broken, p.closed
	p.mu.Unlock()
	if broken != nil {
		return broken
	}
	if closed {
		return ErrClosed
	}
	if len(t.dirty) == 0 {
		return nil
	}
	pages := make([]uint32, 0, len(t.dirty))
	for pgno := range t.dirty {
		pages = append(pages, pgno)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	dbSize := t.PageCount()
	if err := p.w.appendCommit(pages, t.dirty, dbSize, !p.opts.NoSync); err != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.fail(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.w.publish(pages, dbSize)
	if p.w.mxFrame >= p.opts.CheckpointFrames && len(p.readers) == 0 {
		return p.checkpointLocked()
	}
	return nil
}

// Rollback discards the transaction's changes.
func (t *WriteTx) Rollback() {
	if t.done {
		return
	}
	t.done = true
	t.dirty = nil
	t.p.writeMu.Unlock()
}

// ---- cache -----------------------------------------------------------------

type cacheKey struct {
	pgno  uint32
	frame int // -1 for the database file's copy
}

// cache keeps page images with second-chance (clock) eviction.
type cache struct {
	max   int
	pages map[cacheKey]*cacheEntry
	ring  []cacheKey
	hand  int
}

type cacheEntry struct {
	data []byte
	used bool
}

func newCache(max int) *cache { return &cache{max: max, pages: map[cacheKey]*cacheEntry{}} }

func (c *cache) get(k cacheKey) []byte {
	if e, ok := c.pages[k]; ok {
		e.used = true
		return e.data
	}
	return nil
}

func (c *cache) put(k cacheKey, data []byte) {
	if len(c.ring) < c.max {
		c.ring = append(c.ring, k)
		c.pages[k] = &cacheEntry{data: data}
		return
	}
	for {
		victim := c.ring[c.hand]
		e := c.pages[victim]
		if e != nil && e.used {
			e.used = false
			c.hand = (c.hand + 1) % len(c.ring)
			continue
		}
		delete(c.pages, victim)
		c.ring[c.hand] = k
		c.pages[k] = &cacheEntry{data: data}
		c.hand = (c.hand + 1) % len(c.ring)
		return
	}
}

func (c *cache) clear() {
	c.pages = map[cacheKey]*cacheEntry{}
	c.ring = c.ring[:0]
	c.hand = 0
}
