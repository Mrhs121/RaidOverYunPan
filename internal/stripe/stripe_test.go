package stripe_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"sort"
	"sync"
	"testing"

	"github.com/cloudraid/cloudraid/internal/meta"
	"github.com/cloudraid/cloudraid/internal/stripe"
)

// memStore 同时实现 stripe.Sink 和 stripe.Source，把块按 mount/name 存内存里。
type memStore struct {
	mu   sync.Mutex
	data map[string][]byte // key = mount + "|" + name
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) WriteBlock(_ context.Context, mount, name string, size int64, r io.Reader) error {
	buf, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(buf)) != size {
		return io.ErrShortWrite
	}
	m.mu.Lock()
	m.data[mount+"|"+name] = buf
	m.mu.Unlock()
	return nil
}

func (m *memStore) ReadBlock(_ context.Context, mount, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	b, ok := m.data[mount+"|"+name]
	m.mu.Unlock()
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	const total = 10 * 1024 * 1024
	src := make([]byte, total)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	mounts := []string{"/baidu", "/aliyun", "/onedrive", "/local"}

	enc := &stripe.Encoder{
		BlockSize: 1024 * 1024,
		Mounts:    mounts,
		Workers:   4,
		Sink:      store,
	}
	blocks, n, err := enc.Encode(context.Background(), "fid01", bytes.NewReader(src))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n != int64(total) {
		t.Fatalf("encode size: got %d want %d", n, total)
	}
	// round-robin 分布
	wantPerMount := map[string]int{}
	for i := range blocks {
		wantPerMount[mounts[i%len(mounts)]]++
	}
	got := map[string]int{}
	for _, b := range blocks {
		got[b.Mount]++
	}
	for k, v := range wantPerMount {
		if got[k] != v {
			t.Fatalf("mount %s: got %d want %d", k, got[k], v)
		}
	}

	// 块 index 必须连续从 0
	idxs := make([]int, len(blocks))
	for i, b := range blocks {
		idxs[i] = b.Index
	}
	sort.Ints(idxs)
	for i, idx := range idxs {
		if idx != i {
			t.Fatalf("block index gap at %d: %v", i, idxs)
		}
	}

	dec := &stripe.Decoder{Workers: 4, Source: store}
	var out bytes.Buffer
	if err := dec.Decode(context.Background(), blocks, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(out.Bytes(), src) {
		t.Fatalf("round-trip mismatch: out=%d src=%d", out.Len(), len(src))
	}
}

func TestEncodeSmallerThanOneBlock(t *testing.T) {
	store := newMemStore()
	enc := &stripe.Encoder{
		BlockSize: 1024 * 1024,
		Mounts:    []string{"/a", "/b"},
		Sink:      store,
	}
	src := []byte("hello world")
	blocks, n, err := enc.Encode(context.Background(), "fid", bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(src)) || len(blocks) != 1 {
		t.Fatalf("got blocks=%d size=%d", len(blocks), n)
	}
	if blocks[0].Size != int64(len(src)) {
		t.Fatalf("block size %d", blocks[0].Size)
	}
	dec := &stripe.Decoder{Source: store}
	var out bytes.Buffer
	if err := dec.Decode(context.Background(), blocks, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), src) {
		t.Fatal("mismatch")
	}
}

// 防止未使用 import 报警
var _ = meta.Block{}
