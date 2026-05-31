// Package supervisor 负责把 alist 当作子进程拉起来。
//
//   - 通过环境变量 ALIST_ADMIN_PASSWORD 注入初始管理员密码（首次启动时生效）
//   - 在启动前确保 dist 占位文件存在（alist 启动时硬性要求 index.html）
//   - 启动后轮询 /ping 端点，等到 alist 真正能服务再返回
//   - 子进程崩溃时自动重启（带退避）
//   - Stop 优雅关停子进程
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// Options 用来构造 Supervisor。
type Options struct {
	BinaryPath    string // alist 可执行文件
	WorkDir       string // alist 的 --data 目录
	AdminPassword string // 注入到 ALIST_ADMIN_PASSWORD
	Address       string // 检测就绪用的 base URL，例如 http://127.0.0.1:5244
	Stdout        io.Writer
	Stderr        io.Writer
}

// Supervisor 监管 alist 子进程。
type Supervisor struct {
	opt Options

	mu     sync.Mutex
	cmd    *exec.Cmd
	stop   chan struct{}
	doneCh chan struct{}
}

// New 构造一个 Supervisor（不启动）。
func New(opt Options) *Supervisor {
	if opt.Stdout == nil {
		opt.Stdout = os.Stdout
	}
	if opt.Stderr == nil {
		opt.Stderr = os.Stderr
	}
	return &Supervisor{opt: opt}
}

// Start 启动子进程并阻塞直到 alist 就绪（或 ctx 取消）。
// 之后子进程崩溃会被自动重启。
func (s *Supervisor) Start(ctx context.Context) error {
	if _, err := os.Stat(s.opt.BinaryPath); err != nil {
		return fmt.Errorf("alist binary not found: %w", err)
	}
	if err := os.MkdirAll(s.opt.WorkDir, 0o755); err != nil {
		return err
	}
	if err := s.ensureDistPlaceholder(); err != nil {
		return fmt.Errorf("ensure dist placeholder: %w", err)
	}
	s.stop = make(chan struct{})
	s.doneCh = make(chan struct{})
	if err := s.spawn(); err != nil {
		return err
	}
	// 后台守护循环
	go s.loop()

	// 等待端口就绪
	if err := s.waitReady(ctx, 30*time.Second); err != nil {
		_ = s.Stop()
		return fmt.Errorf("alist did not become ready: %w", err)
	}
	return nil
}

// Stop 关停 alist 子进程并停止守护。
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	if s.stop == nil {
		s.mu.Unlock()
		return nil
	}
	close(s.stop)
	s.stop = nil
	cmd := s.cmd
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	<-s.doneCh
	return nil
}

func (s *Supervisor) spawn() error {
	if err := s.ensureDistPlaceholder(); err != nil {
		log.Warnf("supervisor: ensure dist placeholder: %v", err)
	}
	cmd := exec.Command(s.opt.BinaryPath, "server", "--data", s.opt.WorkDir)
	cmd.Env = append(os.Environ(), "ALIST_ADMIN_PASSWORD="+s.opt.AdminPassword)
	cmd.Stdout = s.opt.Stdout
	cmd.Stderr = s.opt.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start alist: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
	log.Infof("supervisor: alist started pid=%d", cmd.Process.Pid)
	return nil
}

func (s *Supervisor) loop() {
	defer close(s.doneCh)
	backoff := time.Second
	for {
		s.mu.Lock()
		cmd := s.cmd
		stopCh := s.stop
		s.mu.Unlock()
		if cmd == nil || stopCh == nil {
			return
		}
		err := cmd.Wait()
		select {
		case <-stopCh:
			log.Infof("supervisor: alist exited (err=%v), shutdown requested", err)
			return
		default:
		}
		log.Warnf("supervisor: alist exited unexpectedly: %v, restarting in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err := s.spawn(); err != nil {
			log.Errorf("supervisor: respawn failed: %v", err)
			return
		}
		backoff = time.Second
	}
}

// waitReady 通过 TCP 连接 + HTTP /ping 探测 alist 是否就绪。
func (s *Supervisor) waitReady(ctx context.Context, total time.Duration) error {
	u, err := url.Parse(s.opt.Address)
	if err != nil {
		return err
	}
	host := u.Host
	if !hasPort(host) {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	deadline := time.Now().Add(total)
	httpClient := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp", host, time.Second)
		if err == nil {
			conn.Close()
			// 再尝试 /ping
			resp, err := httpClient.Get(s.opt.Address + "/ping")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode/100 == 2 {
					return nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", s.opt.Address)
}

func hasPort(host string) bool {
	for i := len(host) - 1; i >= 0; i-- {
		switch host[i] {
		case ':':
			return true
		case ']':
			return false
		}
	}
	return false
}

// ensureDistPlaceholder 让 alist 的 dist 检查通过：
//
// alist 启动时会强制读 dist/index.html，否则 FATA。我们不需要前端，所以
// 在 work_dir 下放一个占位 index.html，并把 config.json 里的 dist_dir 指过去。
// 如果 config.json 还不存在（首启），下次启动 alist 自己会重写一次，那时再补一次。
func (s *Supervisor) ensureDistPlaceholder() error {
	distDir := filepath.Join(s.opt.WorkDir, "public", "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(distDir, "index.html")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		const placeholder = `<!doctype html><html><head><title>cloudraid</title></head>
<body>cloudraid backend - alist API only</body></html>
`
		if err := os.WriteFile(indexPath, []byte(placeholder), 0o644); err != nil {
			return err
		}
	}
	// 把 dist_dir 写进 alist 的 config.json（如果文件存在）
	cfgPath := filepath.Join(s.opt.WorkDir, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		// 不存在没关系：alist 第一次启动会生成它，由我们再次调用更新
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m["dist_dir"] == distDir {
		return nil
	}
	m["dist_dir"] = distDir
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, out, 0o644)
}
