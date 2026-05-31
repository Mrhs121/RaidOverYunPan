package stripe

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/cloudraid/cloudraid/internal/meta"
)

// Sink 是写下游接口。每块独立调用一次。
type Sink interface {
	WriteBlock(ctx context.Context, mount, name string, size int64, r io.Reader) error
}

// Source 是读下游接口；调用方负责关闭返回的 ReadCloser。
type Source interface {
	ReadBlock(ctx context.Context, mount, name string) (io.ReadCloser, error)
}

// Encoder 负责切块 + 分发。
type Encoder struct {
	BlockSize int64
	Mounts    []string
	Workers   int
	Sink      Sink
}

// Encode 读取 src 全部字节，切片后并行写出。返回元数据块清单。
//
// fileID 是本次文件的随机 ID，用于生成块名（fileID.b00, .b01, ...）。
func (e *Encoder) Encode(ctx context.Context, fileID string, src io.Reader) ([]meta.Block, int64, error) {
	if len(e.Mounts) == 0 {
		return nil, 0, fmt.Errorf("stripe: no mounts")
	}
	workers := e.Workers
	if workers <= 0 {
		workers = len(e.Mounts)
	}

	type job struct {
		idx   int
		mount string
		name  string
		size  int64
		buf   []byte
	}

	jobs := make(chan job, workers)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := e.Sink.WriteBlock(ctx, j.mount, j.name, j.size, newByteReader(j.buf)); err != nil {
					select {
					case errCh <- fmt.Errorf("write block %d to %s: %w", j.idx, j.mount, err):
					default:
					}
					cancel()
				}
			}
		}()
	}

	var blocks []meta.Block
	var total int64
	idx := 0
	for {
		buf := make([]byte, e.BlockSize)
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			mount := e.Mounts[idx%len(e.Mounts)]
			name := blockName(fileID, idx)
			blocks = append(blocks, meta.Block{
				Index: idx,
				Mount: mount,
				Name:  name,
				Size:  int64(n),
			})
			total += int64(n)
			select {
			case jobs <- job{idx: idx, mount: mount, name: name, size: int64(n), buf: buf[:n]}:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				if e := <-errCh; e != nil {
					return nil, 0, e
				}
				return nil, 0, ctx.Err()
			}
			idx++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			close(jobs)
			wg.Wait()
			return nil, 0, fmt.Errorf("read source: %w", err)
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return nil, 0, err
	default:
	}
	return blocks, total, nil
}

// Decoder 并行预取块并按顺序写到 dst。
type Decoder struct {
	Workers int
	Source  Source
}

// Decode 用有限窗口并发下载 blocks，并严格按顺序写到 dst。
//
// 窗口大小受 Workers 控制：只会提前下载少量块，避免几百个块同时排队，
// 也避免云盘直链/重定向请求在队列中等待过久导致过期或被限流。
func (d *Decoder) Decode(ctx context.Context, blocks []meta.Block, dst io.Writer) error {
	workers := d.Workers
	if workers <= 0 {
		workers = 2
	}
	if workers > 4 {
		workers = 4
	}
	if workers > len(blocks) {
		workers = len(blocks)
	}
	if workers == 0 {
		return nil
	}

	type slot struct {
		ready chan struct{}
		data  []byte
		err   error
	}

	slots := make([]slot, len(blocks))
	for i := range slots {
		slots[i].ready = make(chan struct{})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := func(i int) {
		go func() {
			defer close(slots[i].ready)
			rc, err := d.Source.ReadBlock(ctx, blocks[i].Mount, blocks[i].Name)
			if err != nil {
				slots[i].err = err
				return
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				slots[i].err = err
				return
			}
			if int64(len(data)) != blocks[i].Size {
				slots[i].err = fmt.Errorf("block %d size mismatch: got %d want %d", blocks[i].Index, len(data), blocks[i].Size)
				return
			}
			slots[i].data = data
		}()
	}

	nextStart := 0
	for nextStart < workers {
		start(nextStart)
		nextStart++
	}

	for i := range blocks {
		select {
		case <-slots[i].ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if slots[i].err != nil {
			cancel()
			return slots[i].err
		}
		if _, err := dst.Write(slots[i].data); err != nil {
			cancel()
			return err
		}
		slots[i].data = nil
		if nextStart < len(blocks) {
			start(nextStart)
			nextStart++
		}
	}
	return nil
}

func blockName(fileID string, idx int) string {
	return fmt.Sprintf("%s.b%05d", fileID, idx)
}

// byteReader 把一个 []byte 包成 io.Reader。
type byteReader struct {
	buf []byte
	pos int
}

func newByteReader(b []byte) *byteReader { return &byteReader{buf: b} }

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.buf) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.pos:])
	b.pos += n
	return n, nil
}