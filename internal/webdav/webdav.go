// Package webdav 把 raid 引擎封装成 golang.org/x/net/webdav.FileSystem，
// 这样 Finder / Windows 资源管理器可以直接挂载 cloudraid。
//
// 当前是只读 + 整体覆盖写的简化实现：
//   - 读：webdav 客户端发 GET，我们把 raid.Get 的流式输出原样返回
//   - 写：webdav 客户端打开 → 写 → Close，我们在 Close 时把累积内容 Put 到 raid
//   - 不支持任意 offset 写、重命名、Range 写（OS 通常不会强用，先标 TODO）
package webdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cloudraid/cloudraid/internal/meta"
	"github.com/cloudraid/cloudraid/internal/raid"
	"golang.org/x/net/webdav"
)

// FS 实现 webdav.FileSystem。
type FS struct {
	Engine *raid.Engine
}

// NewHandler 返回一个完整的 webdav.Handler。
func NewHandler(engine *raid.Engine, username, password string) http.Handler {
	fs := &FS{Engine: engine}
	dav := &webdav.Handler{
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
	if username == "" {
		return dav
	}
	return basicAuth(username, password, dav)
}

func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="cloudraid"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- webdav.FileSystem 实现 ----------

func (f *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	// 我们没有真目录概念；只要能 Stat 出"目录"就行，这里直接成功。
	return nil
}

func (f *FS) RemoveAll(ctx context.Context, name string) error {
	if err := f.Engine.Remove(ctx, normalize(name)); err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	return errors.New("webdav: rename not supported")
}

func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	entry, err := f.Engine.Stat(ctx, normalize(name))
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return &fileInfo{name: entry.Name, size: entry.Size, dir: entry.IsDir}, nil
}

func (f *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	name = normalize(name)
	if flag&os.O_CREATE != 0 || flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
		return &writeFile{ctx: ctx, fs: f, name: name}, nil
	}
	// 读 / Stat
	entry, err := f.Engine.Stat(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if entry.IsDir {
		return &dirFile{ctx: ctx, fs: f, name: name, entry: *entry}, nil
	}
	rr, file, err := f.Engine.OpenAt(ctx, name)
	if err != nil {
		return nil, err
	}
	return &readFile{
		name:   path.Base(name),
		size:   file.Size,
		reader: rr,
	}, nil
}

// ---------- 文件抽象 ----------

type fileInfo struct {
	name string
	size int64
	dir  bool
}

func (f *fileInfo) Name() string { return f.name }
func (f *fileInfo) Size() int64  { return f.size }
func (f *fileInfo) Mode() os.FileMode {
	if f.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (f *fileInfo) ModTime() time.Time { return time.Now() }
func (f *fileInfo) IsDir() bool        { return f.dir }
func (f *fileInfo) Sys() any           { return nil }

// readFile 是 GET 路径用的只读文件，背后是按需拉块的 RandomReader，
// 支持任意 Seek 与 Range，视频/PDF 等随机访问场景能直接用。
type readFile struct {
	name   string
	size   int64
	reader *raid.RandomReader
}

func (r *readFile) Read(p []byte) (int, error)  { return r.reader.Read(p) }
func (r *readFile) Write(p []byte) (int, error) { return 0, errors.New("read-only") }
func (r *readFile) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}
func (r *readFile) Close() error { return r.reader.Close() }
func (r *readFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, errors.New("not a directory")
}
func (r *readFile) Stat() (os.FileInfo, error) {
	return &fileInfo{name: r.name, size: r.size}, nil
}

// dirFile 是目录抽象，主要给 webdav 的 PROPFIND 用。
type dirFile struct {
	ctx   context.Context
	fs    *FS
	name  string
	entry meta.Entry
	off   int
}

func (d *dirFile) Read(p []byte) (int, error)   { return 0, errors.New("is a directory") }
func (d *dirFile) Write(p []byte) (int, error)  { return 0, errors.New("is a directory") }
func (d *dirFile) Close() error                 { return nil }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, errors.New("dir seek") }

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	entries, err := d.fs.Engine.List(d.ctx, d.name)
	if err != nil {
		return nil, err
	}
	if d.off >= len(entries) {
		if count <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	tail := entries[d.off:]
	if count > 0 && count < len(tail) {
		tail = tail[:count]
	}
	out := make([]os.FileInfo, len(tail))
	for i, e := range tail {
		out[i] = &fileInfo{name: e.Name, size: e.Size, dir: e.IsDir}
	}
	d.off += len(tail)
	return out, nil
}

func (d *dirFile) Stat() (os.FileInfo, error) {
	return &fileInfo{name: path.Base(d.name), dir: true}, nil
}

// writeFile 是写入路径：累积 → Close 时 Put。
type writeFile struct {
	ctx  context.Context
	fs   *FS
	name string
	mu   sync.Mutex
	buf  []byte
	closed bool
}

func (w *writeFile) Read(p []byte) (int, error) { return 0, errors.New("write-only") }

func (w *writeFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *writeFile) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	buf := w.buf
	w.buf = nil
	w.mu.Unlock()
	return w.fs.Engine.Put(w.ctx, w.name, int64(len(buf)), bytes.NewReader(buf))
}

func (w *writeFile) Seek(int64, int) (int64, error) { return 0, errors.New("seek on write") }
func (w *writeFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, errors.New("not a directory")
}
func (w *writeFile) Stat() (os.FileInfo, error) {
	return &fileInfo{name: path.Base(w.name)}, nil
}

func normalize(name string) string {
	if name == "" {
		return "/"
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return path.Clean(name)
}
