package pager

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
)

// The write-ahead log is a header followed by frames, each a page image:
//
//	header (32 bytes): magic, version, page size, checkpoint sequence,
//	                   salt1, salt2, crc of the first 24 bytes, unused
//	frame header (24): page number, database size in pages (non-zero only
//	                   on the last frame of a commit), salt1, salt2, crc,
//	                   unused
//	frame data:        one page
//
// Each frame's crc covers the previous frame's crc, its own header and its
// data, so a frame only counts if every frame before it is intact. The
// salts change at every checkpoint, so frames left over from an earlier
// generation of the log are never mistaken for current ones. Recovery
// replays frames up to the last intact commit frame and ignores the rest.

const (
	walMagic     = 0x6361726e // "carn"
	walVersion   = 1
	walHeaderLen = 32
	frameHeadLen = 24
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type wal struct {
	f        File
	pageSize int
	seq      uint32
	salt1    uint32
	salt2    uint32
	lastCRC  uint32 // crc of the last frame written (or of the header)
	nFrames  int    // frames in the file, committed or not
	// The index of committed frames: frameOf[pgno] lists frame numbers in
	// the order they were written.
	frames  []uint32 // frame number -> page number
	frameOf map[uint32][]int
	mxFrame int    // frames that belong to committed transactions
	dbSize  uint32 // database size in pages after the last commit
}

func frameOffset(pageSize, i int) int64 {
	return walHeaderLen + int64(i)*int64(frameHeadLen+pageSize)
}

// openWAL reads an existing log and replays its committed frames into the
// index, or starts a fresh log.
func openWAL(f File, pageSize int) (*wal, error) {
	w := &wal{f: f, pageSize: pageSize, frameOf: map[uint32][]int{}}
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	if size < walHeaderLen || !w.readHeader() {
		return w, w.reset(0)
	}
	// Replay frames while they check out; remember the last commit.
	buf := make([]byte, frameHeadLen+pageSize)
	crc := w.lastCRC
	var pending []uint32
	for i := 0; ; i++ {
		off := frameOffset(pageSize, i)
		if off+int64(len(buf)) > size {
			break
		}
		if _, err := f.ReadAt(buf, off); err != nil {
			break
		}
		pgno := binary.BigEndian.Uint32(buf[0:])
		dbSize := binary.BigEndian.Uint32(buf[4:])
		if binary.BigEndian.Uint32(buf[8:]) != w.salt1 || binary.BigEndian.Uint32(buf[12:]) != w.salt2 {
			break
		}
		next := frameCRC(crc, buf[:16], buf[frameHeadLen:])
		if binary.BigEndian.Uint32(buf[16:]) != next {
			break
		}
		crc = next
		pending = append(pending, pgno)
		if dbSize != 0 {
			for _, p := range pending {
				w.addFrame(p)
			}
			pending = pending[:0]
			w.mxFrame = len(w.frames)
			w.dbSize = dbSize
			w.lastCRC = crc
		}
	}
	w.nFrames = w.mxFrame
	return w, nil
}

func frameCRC(prev uint32, head, data []byte) uint32 {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], prev)
	c := crc32.Update(0, crcTable, b[:])
	c = crc32.Update(c, crcTable, head)
	return crc32.Update(c, crcTable, data)
}

func (w *wal) readHeader() bool {
	var h [walHeaderLen]byte
	if _, err := w.f.ReadAt(h[:], 0); err != nil {
		return false
	}
	if binary.BigEndian.Uint32(h[0:]) != walMagic || binary.BigEndian.Uint32(h[4:]) != walVersion ||
		int(binary.BigEndian.Uint32(h[8:])) != w.pageSize {
		return false
	}
	if crc32.Checksum(h[:24], crcTable) != binary.BigEndian.Uint32(h[24:]) {
		return false
	}
	w.seq = binary.BigEndian.Uint32(h[12:])
	w.salt1 = binary.BigEndian.Uint32(h[16:])
	w.salt2 = binary.BigEndian.Uint32(h[20:])
	w.lastCRC = binary.BigEndian.Uint32(h[24:])
	return true
}

// reset empties the log and starts a new generation with fresh salts.
func (w *wal) reset(seq uint32) error {
	w.seq = seq
	w.salt1 = rand.Uint32()
	w.salt2 = rand.Uint32()
	var h [walHeaderLen]byte
	binary.BigEndian.PutUint32(h[0:], walMagic)
	binary.BigEndian.PutUint32(h[4:], walVersion)
	binary.BigEndian.PutUint32(h[8:], uint32(w.pageSize))
	binary.BigEndian.PutUint32(h[12:], seq)
	binary.BigEndian.PutUint32(h[16:], w.salt1)
	binary.BigEndian.PutUint32(h[20:], w.salt2)
	w.lastCRC = crc32.Checksum(h[:24], crcTable)
	binary.BigEndian.PutUint32(h[24:], w.lastCRC)
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	if _, err := w.f.WriteAt(h[:], 0); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	w.frames = nil
	w.frameOf = map[uint32][]int{}
	w.mxFrame = 0
	w.nFrames = 0
	return nil
}

func (w *wal) addFrame(pgno uint32) {
	w.frameOf[pgno] = append(w.frameOf[pgno], len(w.frames))
	w.frames = append(w.frames, pgno)
}

// appendCommit writes one transaction's pages as frames. The last frame
// carries the new database size, which marks the commit. The frames only
// enter the index (become visible) once publish is called.
func (w *wal) appendCommit(pages []uint32, data map[uint32][]byte, dbSize uint32, sync bool) error {
	buf := make([]byte, frameHeadLen+w.pageSize)
	crc := w.lastCRC
	for i, pgno := range pages {
		binary.BigEndian.PutUint32(buf[0:], pgno)
		size := uint32(0)
		if i == len(pages)-1 {
			size = dbSize
		}
		binary.BigEndian.PutUint32(buf[4:], size)
		binary.BigEndian.PutUint32(buf[8:], w.salt1)
		binary.BigEndian.PutUint32(buf[12:], w.salt2)
		copy(buf[frameHeadLen:], data[pgno])
		crc = frameCRC(crc, buf[:16], buf[frameHeadLen:])
		binary.BigEndian.PutUint32(buf[16:], crc)
		if _, err := w.f.WriteAt(buf, frameOffset(w.pageSize, w.nFrames+i)); err != nil {
			return err
		}
	}
	if sync {
		if err := w.f.Sync(); err != nil {
			return err
		}
	}
	w.lastCRC = crc
	w.nFrames += len(pages)
	return nil
}

func (w *wal) publish(pages []uint32, dbSize uint32) {
	for _, p := range pages {
		w.addFrame(p)
	}
	w.mxFrame = len(w.frames)
	w.dbSize = dbSize
}

// find returns the newest frame for pgno below limit, or -1.
func (w *wal) find(pgno uint32, limit int) int {
	list := w.frameOf[pgno]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] < limit {
			return list[i]
		}
	}
	return -1
}

func (w *wal) readFrame(i int, dst []byte) error {
	_, err := w.f.ReadAt(dst, frameOffset(w.pageSize, i)+frameHeadLen)
	if errors.Is(err, io.EOF) {
		return errors.New("wal: frame past end of log")
	}
	return err
}
