package raid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cloudraid/cloudraid/internal/meta"
)

// RandomReader 是一个按需拉块的逻辑文件读流，支持 Seek 与随机读。
//
// 内部维护一个"当前块缓存"：每次 ReadAt 落到哪个 block，就把那块整段拉下来
// （命中 cache.Cache 优先），后续在同一块内的请求都从内存切片返回。
//
// 适合 mp4 之类需要先 seek 到文件尾再跳到开头读 moov box 的场景：跳尾只
// 下载最后一块（默认 4MiB），代价极小。
type RandomReader struct {
	ctx    context.Context
	source *alistSource
	file   *meta.File

	mu         sync.Mutex
	closed     bool
	curBlock   int   // 当前缓存块的 index，-1 表示无缓存
	curBlockBuf []byte
	pos        int64 // Seek 维护的逻辑位置
}

// OpenAt 打开虚拟文件，返回 RandomReader。
func (e *Engine) OpenAt(ctx context.Context, virtPath string) (*RandomReader, *meta.File, error) {
	f, err := e.Meta.Get(ctx, virtPath)
	if err != nil {
		return nil, nil, err
	}
	return &RandomReader{
		ctx:      ctx,
		source:   &alistSource{client: e.Alist, subdir: e.Subdir, cache: e.Cache},
		file:     f,
		curBlock: -1,
	}, f, nil
}

// Size 返回文件总大小。
func (r *RandomReader) Size() int64 { return r.file.Size }

// Read 实现 io.Reader（基于 pos）。
func (r *RandomReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	pos := r.pos
	r.mu.Unlock()
	if pos >= r.file.Size {
		return 0, io.EOF
	}
	n, err := r.ReadAt(p, pos)
	r.mu.Lock()
	r.pos += int64(n)
	r.mu.Unlock()
	// io.Reader 允许 (n>0, io.EOF)，但消费方对 (n>0, nil) 后再 (0, EOF) 更稳。
	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}
	return n, err
}

// Seek 实现 io.Seeker。支持任意 whence。
func (r *RandomReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errors.New("read closed file")
	}
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.file.Size + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = newPos
	return newPos, nil
}

// ReadAt 实现 io.ReaderAt。线程安全。
//
// 语义遵循 io.ReaderAt：
//   - 若读到文件末尾，必须返回 n>=0 + io.EOF（即使 n>0）
//   - 若 p 没读满但还没到末尾，仍可能返回 n + nil
func (r *RandomReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.file.Size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	hitEOF := false
	if end >= r.file.Size {
		end = r.file.Size
		hitEOF = true
	}

	read := 0
	for off < end {
		blockIdx := int(off / r.file.BlockSize)
		blockStart := int64(blockIdx) * r.file.BlockSize
		blockOff := int(off - blockStart)

		buf, err := r.fetchBlock(blockIdx)
		if err != nil {
			if read > 0 {
				return read, err
			}
			return 0, err
		}
		want := int(end - off)
		if want > len(buf)-blockOff {
			want = len(buf) - blockOff
		}
		copy(p[read:read+want], buf[blockOff:blockOff+want])
		read += want
		off += int64(want)
	}
	if hitEOF {
		return read, io.EOF
	}
	return read, nil
}

// fetchBlock 拿到指定 idx 的块字节，命中本地缓存优先。
func (r *RandomReader) fetchBlock(idx int) ([]byte, error) {
	r.mu.Lock()
	if r.curBlock == idx && r.curBlockBuf != nil {
		buf := r.curBlockBuf
		r.mu.Unlock()
		return buf, nil
	}
	r.mu.Unlock()

	if idx < 0 || idx >= len(r.file.Blocks) {
		return nil, fmt.Errorf("block idx %d out of range [0,%d)", idx, len(r.file.Blocks))
	}
	b := r.file.Blocks[idx]
	rc, err := r.source.ReadBlock(r.ctx, b.Mount, b.Name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != b.Size {
		return nil, fmt.Errorf("block %d size mismatch: got %d want %d", idx, len(data), b.Size)
	}
	r.mu.Lock()
	r.curBlock = idx
	r.curBlockBuf = data
	r.mu.Unlock()
	return data, nil
}

// Close 释放内存缓存。
func (r *RandomReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.curBlockBuf = nil
	return nil
}
