package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config 是 cloudraid 的整体配置。
type Config struct {
	DataDir string `yaml:"data_dir"`

	Alist  AlistConfig  `yaml:"alist"`
	Stripe StripeConfig `yaml:"stripe"`
	Cache  CacheConfig  `yaml:"cache"`
	WebDAV WebDAVConfig `yaml:"webdav"`
}

// AlistConfig 描述如何启动 / 连接 alist。
type AlistConfig struct {
	BinaryPath    string   `yaml:"binary_path"`
	WorkDir       string   `yaml:"work_dir"`
	Address       string   `yaml:"address"`
	AdminUser     string   `yaml:"admin_user"`
	AdminPassword string   `yaml:"admin_password"`
	Mounts        []string `yaml:"mounts"`
	Subdir        string   `yaml:"subdir"`
}

// StripeConfig 是 RAID 0 条带参数。
type StripeConfig struct {
	BlockSize    int64 `yaml:"block_size"`
	WriteWorkers int   `yaml:"write_workers"`
	ReadWorkers  int   `yaml:"read_workers"`
}

// CacheConfig 是本地 L1 缓存配置。
type CacheConfig struct {
	Dir       string `yaml:"dir"`
	MaxBytes  int64  `yaml:"max_bytes"`
	WriteThru bool   `yaml:"write_through"`
}

// WebDAVConfig 暴露给 Finder / 资源管理器的服务端口与凭据。
type WebDAVConfig struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Load 读取配置文件并填充默认值。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Alist.Address == "" {
		c.Alist.Address = "http://127.0.0.1:5244"
	}
	if c.Alist.AdminUser == "" {
		c.Alist.AdminUser = "admin"
	}
	if c.Alist.WorkDir == "" {
		c.Alist.WorkDir = filepath.Join(c.DataDir, "alist")
	}
	if c.Alist.Subdir == "" {
		c.Alist.Subdir = "cloudraid"
	}
	if c.Stripe.BlockSize <= 0 {
		c.Stripe.BlockSize = 4 * 1024 * 1024
	}
	if c.Stripe.WriteWorkers <= 0 {
		c.Stripe.WriteWorkers = len(c.Alist.Mounts)
	}
	if c.Stripe.ReadWorkers <= 0 {
		c.Stripe.ReadWorkers = len(c.Alist.Mounts)
	}
	if c.Cache.Dir == "" {
		c.Cache.Dir = filepath.Join(c.DataDir, "cache")
	}
	if c.Cache.MaxBytes <= 0 {
		c.Cache.MaxBytes = 2 * 1024 * 1024 * 1024
	}
	if c.WebDAV.Listen == "" {
		c.WebDAV.Listen = ":5260"
	}
}

func (c *Config) validate() error {
	if len(c.Alist.Mounts) < 2 {
		return fmt.Errorf("alist.mounts must contain at least 2 entries (got %d)", len(c.Alist.Mounts))
	}
	if c.Alist.AdminPassword == "" {
		return fmt.Errorf("alist.admin_password is required")
	}
	return nil
}
