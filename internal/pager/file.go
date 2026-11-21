// Package pager stores fixed-size pages in a database file, with a
// write-ahead log that makes commits atomic and durable, and gives readers
// consistent snapshots while a writer works.
package pager

import (
	"io"
	"math/rand"
	"os"
	"sync"
)

// File is what the pager needs from a file. OS files satisfy it through
// OpenFile; MemFS provides in-memory files that can simulate a crash.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Close() error
}

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// OpenFile opens (creating if needed) a file on disk.
func OpenFile(path string) (File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

// MemFS is a set of in-memory files for tests. It remembers what was
// synced, so Crash can produce the state a disk might be left in after a
// power cut: everything synced, plus some prefix of the unsynced writes,
// the last of which may be torn.
type MemFS struct {
	mu    sync.Mutex
	files map[string]*MemFile
}

func NewMemFS() *MemFS { return &MemFS{files: map[string]*MemFile{}} }

// Open returns the named file, creating it empty.
func (fs *MemFS) Open(name string) *MemFile {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.files[name]
	if !ok {
		f = &MemFile{}
		fs.files[name] = f
	}
	f.closed = false
	return f
}

// Crash returns a new MemFS holding what survives a crash right now.
func (fs *MemFS) Crash(r *rand.Rand) *MemFS {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := NewMemFS()
	for name, f := range fs.files {
		out.files[name] = f.crashed(r)
	}
	return out
}

type write struct {
	off   int64
	data  []byte
	size  int64 // for truncates: data is nil and size is the new length
	trunc bool
}

// MemFile is one in-memory file.
type MemFile struct {
	mu       sync.Mutex
	data     []byte
	synced   []byte
	unsynced []write
	closed   bool
	// FailAfter, when positive, makes the n-th following write fail (to
	// test I/O errors).
	FailAfter int
}

func (f *MemFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *MemFile) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailAfter > 0 {
		f.FailAfter--
		if f.FailAfter == 0 {
			return 0, io.ErrShortWrite
		}
	}
	apply(&f.data, write{off: off, data: p})
	f.unsynced = append(f.unsynced, write{off: off, data: append([]byte(nil), p...)})
	return len(p), nil
}

func (f *MemFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(&f.data, write{trunc: true, size: size})
	f.unsynced = append(f.unsynced, write{trunc: true, size: size})
	return nil
}

func (f *MemFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = append([]byte(nil), f.data...)
	f.unsynced = nil
	return nil
}

func (f *MemFile) Size() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.data)), nil
}

func (f *MemFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *MemFile) crashed(r *rand.Rand) *MemFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := append([]byte(nil), f.synced...)
	n := 0
	if len(f.unsynced) > 0 {
		n = r.Intn(len(f.unsynced) + 1)
	}
	for i := 0; i < n; i++ {
		w := f.unsynced[i]
		if i == n-1 && !w.trunc && len(w.data) > 1 && r.Intn(2) == 0 {
			// A torn write: only part of the last write reached the disk.
			w.data = w.data[:r.Intn(len(w.data))]
		}
		apply(&data, w)
	}
	return &MemFile{data: data, synced: append([]byte(nil), data...)}
}

func apply(data *[]byte, w write) {
	if w.trunc {
		if w.size < int64(len(*data)) {
			*data = (*data)[:w.size]
		} else {
			*data = append(*data, make([]byte, w.size-int64(len(*data)))...)
		}
		return
	}
	end := w.off + int64(len(w.data))
	if end > int64(len(*data)) {
		*data = append(*data, make([]byte, end-int64(len(*data)))...)
	}
	copy((*data)[w.off:], w.data)
}
