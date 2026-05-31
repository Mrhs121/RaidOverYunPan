// Package cache 是 cloudraid 的本地 L1 块缓存：
//
//   - 命中即直接 io.ReadCloser，避免走云端
//   - 未命中由调用方拉取后再 Put 进来
//   - 容量上限触发时按 LRU 淘汰旧块
//
// 缓存键就是 alist 上的相对块文件名（含我们自己的 ID 前缀），全局唯一。
package cache

import (
	"container/list"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Cache 是线程安全的本地块缓存。
type Cache struct {
	dir      string
	maxBytes int64

	mu        sync.Mutex
	lru       *list.List // 元素是 *entry，最近使用在头部
	index     map[string]*list.Element
	totalSize int64
}

type entry struct {
	key  string
	size int64
}

// Open 在 dir 上构造缓存，必要时扫描已有文件恢复索引。
func Open(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		lru:      list.New(),
		index:    map[string]*list.Element{},
	}
	// 简单恢复：扫描目录，按 mtime 顺序认作 LRU 顺序
	files, _ := os.ReadDir(dir)
	for _, fi := range files {
		if fi.IsDir() {
			continue
		}
		info, err := fi.Info()
		if err != nil {
			continue
		}
		e := &entry{key: fi.Name(), size: info.Size()}
		el := c.lru.PushBack(e)
		c.index[fi.Name()] = el
		c.totalSize += info.Size()
	}
	c.evictIfNeeded()
	return c, nil
}

// Get 返回 key 对应的本地文件读流；未命中返回 ErrMiss。
func (c *Cache) Get(key string) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	el, ok := c.index[key]
	if !ok {
		c.mu.Unlock()
		return nil, 0, ErrMiss
	}
	c.lru.MoveToFront(el)
	size := el.Value.(*entry).size
	path := filepath.Join(c.dir, key)
	c.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		// 索引和磁盘不一致：清掉这条
		c.evict(key)
		return nil, 0, ErrMiss
	}
	return f, size, nil
}

// Put 把 reader 的内容写到本地缓存。返回 size。
func (c *Cache) Put(key string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(c.dir, "tmp-*")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpPath)
		return 0, err
	}
	final := filepath.Join(c.dir, key)
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return 0, err
	}

	c.mu.Lock()
	if old, ok := c.index[key]; ok {
		c.totalSize -= old.Value.(*entry).size
		c.lru.Remove(old)
	}
	e := &entry{key: key, size: n}
	c.index[key] = c.lru.PushFront(e)
	c.totalSize += n
	c.mu.Unlock()

	c.evictIfNeeded()
	return n, nil
}

// Remove 从缓存中移除指定 key（不存在不报错）。
func (c *Cache) Remove(key string) {
	c.evict(key)
}

func (c *Cache) evict(key string) {
	c.mu.Lock()
	el, ok := c.index[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	c.totalSize -= el.Value.(*entry).size
	c.lru.Remove(el)
	delete(c.index, key)
	c.mu.Unlock()
	os.Remove(filepath.Join(c.dir, key))
}

func (c *Cache) evictIfNeeded() {
	for {
		c.mu.Lock()
		if c.totalSize <= c.maxBytes {
			c.mu.Unlock()
			return
		}
		oldest := c.lru.Back()
		if oldest == nil {
			c.mu.Unlock()
			return
		}
		e := oldest.Value.(*entry)
		c.lru.Remove(oldest)
		delete(c.index, e.key)
		c.totalSize -= e.size
		c.mu.Unlock()
		os.Remove(filepath.Join(c.dir, e.key))
	}
}

// ErrMiss 表示缓存未命中。
var ErrMiss = errors.New("cache: miss")
