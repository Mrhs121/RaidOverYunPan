// Package meta 持久化"虚拟文件 → 物理块清单"的映射。
//
// 一个虚拟文件被拆成 N 个等大小（最后一块可能更小）的块，每块通过 alist
// 落到某个 mount。我们用 SQLite 存这份清单：虚拟路径作为主键，块按 index 排序。
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Block 是单个条带块的元数据。
type Block struct {
	Index int
	Mount string // alist 的 mount 路径，例如 "/baidu"
	Name  string // 块文件名，例如 "<file-id>.b00"
	Size  int64
}

// File 是一个虚拟文件的完整元数据。
type File struct {
	Path      string // 虚拟路径，例如 /movies/foo.mp4
	Size      int64
	BlockSize int64
	Blocks    []Block
	UpdatedAt time.Time
	ID        string // 内部 ID，块文件名前缀
}

// Store 是元数据存储的句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）SQLite 元数据库。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite 串行写最稳
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			path        TEXT PRIMARY KEY,
			id          TEXT NOT NULL,
			size        INTEGER NOT NULL,
			block_size  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS blocks (
			path     TEXT NOT NULL,
			idx      INTEGER NOT NULL,
			mount    TEXT NOT NULL,
			name     TEXT NOT NULL,
			size     INTEGER NOT NULL,
			PRIMARY KEY(path, idx),
			FOREIGN KEY(path) REFERENCES files(path) ON DELETE CASCADE
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// Put 写入或覆盖一个文件的元数据（含所有块）。
func (s *Store) Put(ctx context.Context, f *File) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM blocks WHERE path = ?`, f.Path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files(path,id,size,block_size,updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET id=excluded.id,size=excluded.size,
		   block_size=excluded.block_size,updated_at=excluded.updated_at`,
		f.Path, f.ID, f.Size, f.BlockSize, time.Now().Unix()); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO blocks(path,idx,mount,name,size) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range f.Blocks {
		if _, err := stmt.ExecContext(ctx, f.Path, b.Index, b.Mount, b.Name, b.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ErrNotFound 表示路径不存在。
var ErrNotFound = errors.New("meta: not found")

// Get 读取虚拟路径的元数据。
func (s *Store) Get(ctx context.Context, virtPath string) (*File, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT path,id,size,block_size,updated_at FROM files WHERE path = ?`, virtPath)
	var f File
	var ts int64
	if err := row.Scan(&f.Path, &f.ID, &f.Size, &f.BlockSize, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.UpdatedAt = time.Unix(ts, 0)
	rows, err := s.db.QueryContext(ctx,
		`SELECT idx,mount,name,size FROM blocks WHERE path = ? ORDER BY idx`, virtPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.Index, &b.Mount, &b.Name, &b.Size); err != nil {
			return nil, err
		}
		f.Blocks = append(f.Blocks, b)
	}
	return &f, rows.Err()
}

// Delete 删除某个虚拟路径的元数据，返回被删的块清单（调用方拿去清理云端）。
func (s *Store) Delete(ctx context.Context, virtPath string) (*File, error) {
	f, err := s.Get(ctx, virtPath)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, virtPath); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blocks WHERE path = ?`, virtPath); err != nil {
		return nil, err
	}
	return f, nil
}

// Entry 是 List 的返回项。
type Entry struct {
	Name  string
	IsDir bool
	Size  int64
}

// List 列出某个虚拟目录下的直接子项（虚拟目录不真实存在，从 path 前缀推导）。
func (s *Store) List(ctx context.Context, dir string) ([]Entry, error) {
	if !strings.HasSuffix(dir, "/") {
		dir = dir + "/"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT path,size FROM files WHERE path LIKE ? ORDER BY path`, dir+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []Entry
	for rows.Next() {
		var p string
		var size int64
		if err := rows.Scan(&p, &size); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(p, dir)
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			name := rest[:i]
			if !seen[name] {
				seen[name] = true
				out = append(out, Entry{Name: name, IsDir: true})
			}
		} else {
			out = append(out, Entry{Name: rest, Size: size})
		}
	}
	return out, rows.Err()
}

// Stat 是 webdav 用的轻量查询。
func (s *Store) Stat(ctx context.Context, virtPath string) (*Entry, error) {
	if virtPath == "/" || virtPath == "" {
		return &Entry{Name: "/", IsDir: true}, nil
	}
	f, err := s.Get(ctx, virtPath)
	if err == nil {
		return &Entry{Name: path.Base(f.Path), Size: f.Size}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// 把它当成目录看：只要有任何文件以 virtPath/ 为前缀就视为存在
	prefix := strings.TrimSuffix(virtPath, "/") + "/"
	row := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM files WHERE path LIKE ? LIMIT 1`, prefix+"%")
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Entry{Name: path.Base(virtPath), IsDir: true}, nil
}

// 占位，避免空 import；保留 strings 在文件顶部
var _ = strings.HasSuffix

