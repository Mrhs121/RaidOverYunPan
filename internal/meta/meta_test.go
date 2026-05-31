package meta_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudraid/cloudraid/internal/meta"
)

func openStore(t *testing.T) *meta.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetDelete(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := &meta.File{
		Path:      "/movies/foo.mp4",
		ID:        "abcd",
		Size:      1024,
		BlockSize: 256,
		Blocks: []meta.Block{
			{Index: 0, Mount: "/baidu", Name: "abcd.b00", Size: 256},
			{Index: 1, Mount: "/aliyun", Name: "abcd.b01", Size: 256},
			{Index: 2, Mount: "/onedrive", Name: "abcd.b02", Size: 256},
			{Index: 3, Mount: "/local", Name: "abcd.b03", Size: 256},
		},
	}
	if err := s.Put(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "/movies/foo.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 1024 || len(got.Blocks) != 4 {
		t.Fatalf("got %+v", got)
	}
	if got.Blocks[2].Mount != "/onedrive" {
		t.Fatalf("blocks order broken: %+v", got.Blocks)
	}

	if _, err := s.Delete(ctx, "/movies/foo.mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "/movies/foo.mp4"); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestList(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	mk := func(p string) {
		if err := s.Put(ctx, &meta.File{Path: p, ID: "x", Size: 1, BlockSize: 1}); err != nil {
			t.Fatal(err)
		}
	}
	mk("/a/b/c.txt")
	mk("/a/d.txt")
	mk("/a/e.txt")
	es, err := s.List(ctx, "/a")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range es {
		got[e.Name] = e.IsDir
	}
	if !got["b"] || got["d.txt"] || !!got["d.txt"] != true {
		// 等价检查：b 应是目录, d.txt 应是文件
	}
	if v, ok := got["b"]; !ok || !v {
		t.Fatalf("expected dir b, got %+v", got)
	}
	if v, ok := got["d.txt"]; !ok || v {
		t.Fatalf("expected file d.txt, got %+v", got)
	}
}

func TestStat(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, &meta.File{Path: "/x/y.txt", ID: "x", Size: 7, BlockSize: 1}); err != nil {
		t.Fatal(err)
	}
	// 文件
	e, err := s.Stat(ctx, "/x/y.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.IsDir || e.Size != 7 {
		t.Fatalf("file stat: %+v", e)
	}
	// 推断目录
	e, err = s.Stat(ctx, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsDir {
		t.Fatalf("expected dir, got %+v", e)
	}
	// 不存在
	if _, err := s.Stat(ctx, "/no/such"); err == nil {
		t.Fatal("expected not found")
	}
}
