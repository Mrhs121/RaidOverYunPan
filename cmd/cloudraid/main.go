// cloudraid 命令行入口。子命令：
//
//	cloudraid -config config.yaml put <local> <virt>
//	cloudraid -config config.yaml get <virt> <local>
//	cloudraid -config config.yaml ls  <virt-dir>
//	cloudraid -config config.yaml rm  <virt>
//	cloudraid -config config.yaml serve   # 启动 alist + WebDAV
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cloudraid/cloudraid/internal/alist"
	"github.com/cloudraid/cloudraid/internal/cache"
	"github.com/cloudraid/cloudraid/internal/config"
	"github.com/cloudraid/cloudraid/internal/meta"
	"github.com/cloudraid/cloudraid/internal/raid"
	"github.com/cloudraid/cloudraid/internal/supervisor"
	cloudwebdav "github.com/cloudraid/cloudraid/internal/webdav"
)

func main() {
	cfgPath := flag.String("config", "data/config.yaml", "配置文件路径")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	switch args[0] {
	case "put":
		if len(args) != 3 {
			usage()
			os.Exit(2)
		}
		runWithEngine(ctx, cfg, true, func(e *raid.Engine) error {
			return cmdPut(ctx, e, args[1], args[2])
		})
	case "get":
		if len(args) != 3 {
			usage()
			os.Exit(2)
		}
		runWithEngine(ctx, cfg, false, func(e *raid.Engine) error {
			return cmdGet(ctx, e, args[1], args[2])
		})
	case "ls":
		dir := "/"
		if len(args) >= 2 {
			dir = args[1]
		}
		runWithEngine(ctx, cfg, false, func(e *raid.Engine) error {
			return cmdLs(ctx, e, dir)
		})
	case "rm":
		if len(args) != 2 {
			usage()
			os.Exit(2)
		}
		runWithEngine(ctx, cfg, false, func(e *raid.Engine) error {
			return e.Remove(ctx, args[1])
		})
	case "serve":
		runWithEngine(ctx, cfg, false, func(e *raid.Engine) error {
			return cmdServe(ctx, cfg, e)
		})
	default:
		usage()
		os.Exit(2)
	}
}

// runWithEngine 启动 alist supervisor、登录、构造 engine，并在动作完成后关停。
//
// 如果 cfg.Alist.Address 上已经有 alist 在跑（例如 cloudraid serve 已起），
// 直接复用，不再 fork 一份子进程。
func runWithEngine(ctx context.Context, cfg *config.Config, prepare bool, fn func(*raid.Engine) error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fatal("mkdir data: %v", err)
	}

	if !alistAlive(cfg.Alist.Address) {
		sup := supervisor.New(supervisor.Options{
			BinaryPath:    cfg.Alist.BinaryPath,
			WorkDir:       cfg.Alist.WorkDir,
			AdminPassword: cfg.Alist.AdminPassword,
			Address:       cfg.Alist.Address,
		})
		if err := sup.Start(ctx); err != nil {
			fatal("start alist: %v", err)
		}
		defer sup.Stop()
	}

	client := alist.New(cfg.Alist.Address, cfg.Alist.AdminUser, cfg.Alist.AdminPassword)
	if err := client.Login(ctx); err != nil {
		fatal("alist login: %v (提示：第一次启动时 alist 会用 ALIST_ADMIN_PASSWORD 写入新密码；如果之前用过别的密码，请改 admin_password 或者删除 %s/data.db 重置)", err, cfg.Alist.WorkDir)
	}

	cch, err := cache.Open(cfg.Cache.Dir, cfg.Cache.MaxBytes)
	if err != nil {
		fatal("open cache: %v", err)
	}
	mst, err := meta.Open(filepath.Join(cfg.DataDir, "meta.db"))
	if err != nil {
		fatal("open meta: %v", err)
	}
	defer mst.Close()

	engine := &raid.Engine{
		Alist:        client,
		Cache:        cch,
		Meta:         mst,
		Mounts:       cfg.Alist.Mounts,
		Subdir:       cfg.Alist.Subdir,
		Block:        cfg.Stripe.BlockSize,
		Workers:      cfg.Stripe.WriteWorkers,
		WriteThrough: cfg.Cache.WriteThru,
	}
	if prepare {
		if err := engine.PrepareMounts(ctx); err != nil {
			fatal("prepare mounts: %v", err)
		}
	}
	if err := fn(engine); err != nil {
		fatal("%v", err)
	}
}

func cmdPut(ctx context.Context, e *raid.Engine, local, virt string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	log.Infof("put %s (%d bytes) → %s, splitting into %d-byte blocks across %d mounts",
		local, st.Size(), virt, e.Block, len(e.Mounts))
	if err := e.Put(ctx, virt, st.Size(), f); err != nil {
		return err
	}
	fmt.Println("OK")
	return nil
}

func cmdGet(ctx context.Context, e *raid.Engine, virt, local string) error {
	rc, size, err := e.Get(ctx, virt)
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(local)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, rc)
	if err != nil {
		return err
	}
	if size > 0 && n != size {
		return fmt.Errorf("short read: got %d want %d", n, size)
	}
	fmt.Printf("OK %d bytes\n", n)
	return nil
}

func cmdLs(ctx context.Context, e *raid.Engine, dir string) error {
	entries, err := e.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir {
			fmt.Printf("d %s/\n", e.Name)
		} else {
			fmt.Printf("f %12d %s\n", e.Size, e.Name)
		}
	}
	return nil
}

func cmdServe(ctx context.Context, cfg *config.Config, e *raid.Engine) error {
	handler := cloudwebdav.NewHandler(e, cfg.WebDAV.Username, cfg.WebDAV.Password)
	server := &http.Server{Addr: cfg.WebDAV.Listen, Handler: handler}

	errCh := make(chan error, 1)
	go func() {
		log.Infof("webdav serving on %s (user=%s)", cfg.WebDAV.Listen, cfg.WebDAV.Username)
		errCh <- server.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Infof("got signal %s, shutting down", s)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	_ = server.Shutdown(shutCtx)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  cloudraid -config <path> put <local-file> <virt-path>
  cloudraid -config <path> get <virt-path> <local-file>
  cloudraid -config <path> ls  [virt-dir]
  cloudraid -config <path> rm  <virt-path>
  cloudraid -config <path> serve`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cloudraid: "+format+"\n", args...)
	os.Exit(1)
}

// alistAlive 探测 base 上是否已经有 alist 监听 /ping。
func alistAlive(base string) bool {
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(base + "/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode/100 == 2
}
