package cache_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cloudraid/cloudraid/internal/cache"
)

func TestPutGet(t *testing.T) {
	c, err := cache.Open(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put("k1", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, size, err := c.Get("k1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if size != 5 {
		t.Fatalf("size %d", size)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("got %q", got)
	}
}

func TestMiss(t *testing.T) {
	c, _ := cache.Open(t.TempDir(), 1024)
	if _, _, err := c.Get("none"); err != cache.ErrMiss {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestEvictByLRU(t *testing.T) {
	c, _ := cache.Open(t.TempDir(), 6) // 只放得下 6 字节
	c.Put("a", strings.NewReader("aaa"))
	c.Put("b", strings.NewReader("bbb"))
	// 提升 a 的最近使用
	rc, _, _ := c.Get("a")
	rc.Close()
	// 加 c → 容量不够，b 应被淘汰
	c.Put("c", strings.NewReader("ccc"))

	if _, _, err := c.Get("b"); err != cache.ErrMiss {
		t.Fatalf("expected b evicted, got err=%v", err)
	}
	if _, _, err := c.Get("a"); err != nil {
		t.Fatalf("a should still be cached: %v", err)
	}
	if _, _, err := c.Get("c"); err != nil {
		t.Fatalf("c should be cached: %v", err)
	}
}

func TestRemove(t *testing.T) {
	c, _ := cache.Open(t.TempDir(), 1024)
	c.Put("k", strings.NewReader("v"))
	c.Remove("k")
	if _, _, err := c.Get("k"); err != cache.ErrMiss {
		t.Fatal("expected miss after remove")
	}
}
