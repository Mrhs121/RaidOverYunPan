// Package raid 是 cloudraid 的核心引擎，把 stripe / cache / alist / meta 串起来。
//
// 对外提供 4 个动作：
//
//	Put    虚拟路径 + 文件大小 + reader → 切块并发分发
//	Get    虚拟路径 → 顺序拼接的 reader
//	Remove 虚拟路径 → 删 alist 上的所有块 + meta
//	List   虚拟目录 → 子项
//
// 缓存策略：
//   - 写入时如果开启 write_through，每一块在上传成功后立刻写入本地缓存
//   - 读取时优先查缓存；未命中再走 alist 直链
package raid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cloudraid/cloudraid/internal/alist"
	"github.com/cloudraid/cloudraid/internal/cache"
	"github.com/cloudraid/cloudraid/internal/meta"
	"github.com/cloudraid/cloudraid/internal/stripe"
)

// Engine 串起所有依赖。
type Engine struct {
	Alist   *alist.Client
	Cache   *cache.Cache
	Meta    *meta.Store
	Mounts  []string
	Subdir  string
	Block   int64
	Workers int

	WriteThrough bool
}

// Put 写入文件。size <= 0 表示未知（reader 必须可被读到 EOF）。
func (e *Engine) Put(ctx context.Context, virtPath string, size int64, r io.Reader) error {
	if !strings.HasPrefix(virtPath, "/") {
		virtPath = "/" + virtPath
	}
	// 如果已存在，先删旧的（云端 + meta），再写新的
	if old, err := e.Meta.Get(ctx, virtPath); err == nil {
		_ = e.removeBlocks(ctx, old)
		_, _ = e.Meta.Delete(ctx, virtPath)
	}

	fileID := newFileID()
	enc := &stripe.Encoder{
		BlockSize: e.Block,
		Mounts:    e.Mounts,
		Workers:   e.Workers,
		Sink:      &alistSink{client: e.Alist, subdir: e.Subdir, cache: cacheIfThrough(e)},
	}
	blocks, total, err := enc.Encode(ctx, fileID, r)
	if err != nil {
		// 出错时尽量清理已经上传的部分
		_ = e.removeBlocksRaw(ctx, blocks)
		return err
	}
	if size > 0 && total != size {
		// 调用方声明的 size 与实际读到的不一致：以实际为准，但提示
		// 这里不报错，让上层决定
	}
	return e.Meta.Put(ctx, &meta.File{
		Path:      virtPath,
		ID:        fileID,
		Size:      total,
		BlockSize: e.Block,
		Blocks:    blocks,
	})
}

// Get 返回顺序拼接的读流。调用方必须 Close。
func (e *Engine) Get(ctx context.Context, virtPath string) (io.ReadCloser, int64, error) {
	if !strings.HasPrefix(virtPath, "/") {
		virtPath = "/" + virtPath
	}
	f, err := e.Meta.Get(ctx, virtPath)
	if err != nil {
		return nil, 0, err
	}
	pr, pw := io.Pipe()
	dec := &stripe.Decoder{
		Workers: e.Workers,
		Source:  &alistSource{client: e.Alist, subdir: e.Subdir, cache: e.Cache},
	}
	go func() {
		err := dec.Decode(ctx, f.Blocks, pw)
		_ = pw.CloseWithError(err)
	}()
	return pr, f.Size, nil
}

// Remove 删除虚拟路径及其全部块。
func (e *Engine) Remove(ctx context.Context, virtPath string) error {
	f, err := e.Meta.Delete(ctx, virtPath)
	if err != nil {
		return err
	}
	return e.removeBlocks(ctx, f)
}

// List 列出某虚拟目录的直接子项。
func (e *Engine) List(ctx context.Context, dir string) ([]meta.Entry, error) {
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	return e.Meta.List(ctx, dir)
}

// Stat 单项信息（webdav 用）。
func (e *Engine) Stat(ctx context.Context, p string) (*meta.Entry, error) {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return e.Meta.Stat(ctx, p)
}

// PrepareMounts 在每个 mount 下创建 subdir，方便后续的块文件落地。
func (e *Engine) PrepareMounts(ctx context.Context) error {
	for _, m := range e.Mounts {
		full := alist.JoinPath(m, e.Subdir, "")
		if err := e.Alist.Mkdir(ctx, full); err != nil {
			return fmt.Errorf("mkdir %s: %w", full, err)
		}
	}
	return nil
}

// ---------- internal helpers ----------

func (e *Engine) removeBlocks(ctx context.Context, f *meta.File) error {
	return e.removeBlocksRaw(ctx, f.Blocks)
}

func (e *Engine) removeBlocksRaw(ctx context.Context, blocks []meta.Block) error {
	// 按 mount 分组，一次性提交多个名字
	byMount := map[string][]string{}
	for _, b := range blocks {
		dir := alist.JoinPath(b.Mount, e.Subdir, "")
		byMount[dir] = append(byMount[dir], b.Name)
		if e.Cache != nil {
			e.Cache.Remove(b.Name)
		}
	}
	for dir, names := range byMount {
		if err := e.Alist.Remove(ctx, dir, names); err != nil {
			return err
		}
	}
	return nil
}

func cacheIfThrough(e *Engine) *cache.Cache {
	if e.WriteThrough {
		return e.Cache
	}
	return nil
}

func newFileID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------- sink / source 实现 ----------

type alistSink struct {
	client *alist.Client
	subdir string
	cache  *cache.Cache // 可选：写入即缓存
}

func (s *alistSink) WriteBlock(ctx context.Context, mount, name string, size int64, r io.Reader) error {
	full := alist.JoinPath(mount, s.subdir, name)
	if s.cache != nil {
		// 用 TeeReader 把上传的字节顺手落到本地缓存
		pr, pw := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			_, err := s.cache.Put(name, pr)
			errCh <- err
		}()
		tee := io.TeeReader(r, pw)
		uploadErr := s.client.PutStream(ctx, full, size, tee)
		_ = pw.Close()
		cacheErr := <-errCh
		if uploadErr != nil {
			s.cache.Remove(name)
			return uploadErr
		}
		if cacheErr != nil {
			// 缓存失败不影响主路径
		}
		return nil
	}
	return s.client.PutStream(ctx, full, size, r)
}

type alistSource struct {
	client *alist.Client
	subdir string
	cache  *cache.Cache
}

func (s *alistSource) ReadBlock(ctx context.Context, mount, name string) (io.ReadCloser, error) {
	if s.cache != nil {
		if rc, _, err := s.cache.Get(name); err == nil {
			return rc, nil
		}
	}
	full := alist.JoinPath(mount, s.subdir, name)
	// 最多重试 5 次：阿里云盘直链有效期 15 分钟，排队靠后的块可能签名过期
	// 每次重试会重新调 /api/fs/get 获取新签名
	var data []byte
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			// 重试前等待一小段时间，让 AList 刷新签名缓存
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		rc, err := s.client.Download(ctx, full)
		if err != nil {
			lastErr = err
			log.Debugf("ReadBlock %s attempt %d Download failed: %v, retrying", full, attempt, err)
			continue
		}
		data, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			lastErr = err
			log.Debugf("ReadBlock %s attempt %d ReadAll failed: %v, retrying", full, attempt, err)
			continue
		}
		// 成功
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if s.cache != nil {
		_, _ = s.cache.Put(name, byteReadSeeker(data))
	}
	return io.NopCloser(byteReadSeeker(data)), nil
}

// byteReadSeeker 把 []byte 包成 io.Reader，给 cache.Put 和返回流共用。
func byteReadSeeker(b []byte) *bytesReader { return &bytesReader{buf: b} }

type bytesReader struct {
	buf []byte
	pos int
}

func (b *bytesReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.buf) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.pos:])
	b.pos += n
	return n, nil
}
