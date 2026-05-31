// Package alist 是一个轻量级的 alist HTTP 客户端，仅覆盖 cloudraid 需要的接口：
//   - 登录拿 token（/api/auth/login/hash）
//   - 上传块文件（PUT /api/fs/put，流式）
//   - 取上游直链（POST /api/fs/get → raw_url）
//   - 删除块（POST /api/fs/remove）
//   - 创建子目录（POST /api/fs/mkdir）
//
// 加速场景里我们尽量走直链，让真正的字节传输绕开 alist 进程。
package alist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client 与 alist 服务通信。线程安全。
type Client struct {
	base       string
	user       string
	password   string
	httpClient *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New 构造一个客户端。base 形如 http://127.0.0.1:5244。
func New(base, user, password string) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"),
		user:       user,
		password:   password,
		httpClient: &http.Client{Timeout: 0}, // 上传可能很久，不设全局超时
	}
}

// envelope 是 alist 统一响应封皮。
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// loginResp 是 /api/auth/login/hash 的 data 部分。
type loginResp struct {
	Token string `json:"token"`
}

// Login 强制刷新一次 token。
func (c *Client) Login(ctx context.Context) error {
	hashed := sha256Hex(c.password + "-https://github.com/alist-org/alist")
	body, _ := json.Marshal(map[string]string{
		"username": c.user,
		"password": hashed,
	})
	var lr loginResp
	if err := c.do(ctx, http.MethodPost, "/api/auth/login/hash", nil, bytes.NewReader(body), &lr); err != nil {
		return err
	}
	if lr.Token == "" {
		return fmt.Errorf("alist login: empty token")
	}
	c.mu.Lock()
	c.token = lr.Token
	c.expiresAt = time.Now().Add(40 * time.Minute) // alist 默认 48h，这里保守续期
	c.mu.Unlock()
	return nil
}

// ensureToken 在必要时登录。
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	t, exp := c.token, c.expiresAt
	c.mu.Unlock()
	if t != "" && time.Now().Before(exp) {
		return t, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	t = c.token
	c.mu.Unlock()
	return t, nil
}

// PutStream 把 reader 的 size 字节流式写到 alist 的 fullPath（含 mount 前缀）。
func (c *Client) PutStream(ctx context.Context, fullPath string, size int64, r io.Reader) error {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/fs/put", r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", tok)
	req.Header.Set("File-Path", url.PathEscape(fullPath))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("As-Task", "false")
	req.ContentLength = size
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeEnvelope(resp.Body, nil)
}

// FileInfo 是 /api/fs/get 关心的字段。
type FileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	RawURL   string `json:"raw_url"`
	Provider string `json:"provider"`
	Sign     string `json:"sign"`
}

// Get 返回某个 alist 路径的文件信息（包含 raw_url 直链）。
func (c *Client) Get(ctx context.Context, fullPath string) (*FileInfo, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"path": fullPath})
	var fi FileInfo
	if err := c.do(ctx, http.MethodPost, "/api/fs/get",
		map[string]string{"Authorization": tok}, bytes.NewReader(body), &fi); err != nil {
		return nil, err
	}
	return &fi, nil
}

// Download 下载 fullPath 的全部字节。
//
// 只使用 AList 的 WebDAV 接口：GET /dav/<path> + Basic Auth。
// 这是 AList 对外封装后的统一下载入口，cloudraid 不直接接触百度/阿里 API。
//
// 注意：AList WebDAV handler 会返回 302 到上游 CDN。Go 默认跟随 302 时会
// 自动加 Referer，阿里云盘 CDN 会拒绝带 Referer 的请求。因此这里使用
// 自定义 CheckRedirect，跨域重定向时删除 Authorization 和 Referer。
func (c *Client) Download(ctx context.Context, fullPath string) (io.ReadCloser, error) {
	davURL := c.base + "/dav" + escapePath(fullPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, davURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.password)
	// 阿里云盘 CDN 对 Go 默认 UA 会返回 403，使用 curl UA 与手工验证保持一致。
	req.Header.Set("User-Agent", "curl/8.7.1")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			// 跨域进入网盘 CDN 时，删除本地 AList 的认证信息和 Referer。
			req.Header.Del("Authorization")
			req.Header.Del("Referer")
			// Go 默认 UA 会被部分网盘 CDN 拒绝；保持与 curl 验证路径一致。
			req.Header.Set("User-Agent", "curl/8.7.1")
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("alist webdav GET %s: status %d", fullPath, resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/html") {
		resp.Body.Close()
		return nil, fmt.Errorf("alist webdav GET %s returned non-file content-type %s", fullPath, ct)
	}
	return resp.Body, nil
}

// Remove 删除某个目录下的若干名字。
func (c *Client) Remove(ctx context.Context, dir string, names []string) error {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"dir": dir, "names": names})
	return c.do(ctx, http.MethodPost, "/api/fs/remove",
		map[string]string{"Authorization": tok}, bytes.NewReader(body), nil)
}

// Mkdir 在 alist 下创建目录（幂等：已存在不会报错）。
func (c *Client) Mkdir(ctx context.Context, fullPath string) error {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"path": fullPath})
	err = c.do(ctx, http.MethodPost, "/api/fs/mkdir",
		map[string]string{"Authorization": tok}, bytes.NewReader(body), nil)
	if err != nil && strings.Contains(err.Error(), "exist") {
		return nil
	}
	return err
}

// JoinPath 拼接 mount + subdir + name。
func JoinPath(mount, subdir, name string) string {
	return path.Join("/", strings.Trim(mount, "/"), subdir, name)
}

// ---------- helpers ----------

func (c *Client) do(ctx context.Context, method, urlPath string, headers map[string]string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+urlPath, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeEnvelope(resp.Body, out)
}

func decodeEnvelope(body io.Reader, out any) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (raw=%s)", err, truncate(raw, 200))
	}
	if env.Code != 200 {
		return fmt.Errorf("alist code=%d msg=%s", env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
