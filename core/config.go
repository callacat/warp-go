// Package core 提供可复用的 WARP 客户端核心：配置加载/热重载、注册编排、
// Server 生命周期（Start/Stop）、状态查询。CLI（main.go）与 GUI（gui/）共用，
// 不依赖任何界面层。
package core

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"warp/route"
)

// Config 是 config.json 的运行时配置，位于程序执行目录。
//
// 优先级：命令行旗标 > config.json > 默认值。文件变更（mtime 或内容）触发
// 热重载，见 WatchConfig。
type Config struct {
	// ListenAddr 是 mixed HTTP+SOCKS5 代理的监听地址。
	ListenAddr string `json:"listen_addr"`
	// EdgeAddr 是扫描应用的最优边缘地址（host:port）；空 = 用注册信息默认。
	// GUI 扫描页"应用"按钮写入；下次启动生效。
	EdgeAddr string `json:"edge_addr,omitempty"`
	// RulesPath 是路由规则文件路径（相对执行目录解析）。
	RulesPath string `json:"rules_path"`
	// GeoDir 是 geosite.dat / geoip-lite.dat 所在目录（相对执行目录解析）。
	GeoDir string `json:"geo_dir"`
	// GeoRepo 是 GEO 数据发布仓库；两个下载 URL 由此推导（见 GeoSiteURL/GeoIPURL）。
	GeoRepo string `json:"geo_repo"`
	// GeoAutoUpdateDays 是 GEO 数据自动更新间隔（天），0 表示关闭。
	GeoAutoUpdateDays int `json:"geo_auto_update_days"`
	// EnableSystemProxy 控制是否把系统代理指向 mixed 代理端口。
	EnableSystemProxy bool `json:"enable_system_proxy"`
	// AllowUDP 控制是否响应 SOCKS5 UDP ASSOCIATE（数据报直连，不经隧道）。
	AllowUDP bool `json:"allow_udp"`
}

// DefaultConfig 返回内置默认值。LoadConfig 以它为基底，JSON 反序列化只覆盖
// 文件中出现的字段——因此"缺省"与"显式默认值"等价，"显式 0"仍能关闭自动更新。
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:        "127.0.0.1:40000",
		RulesPath:         "rules.txt",
		GeoDir:            "geo",
		GeoRepo:           "https://github.com/MetaCubeX/meta-rules-dat",
		GeoAutoUpdateDays: 7,
		EnableSystemProxy: false,
		AllowUDP:          false,
	}
}

// GeoSiteURL 由 GeoRepo 推导 geosite.dat 的下载地址；仓库为空时回退到内置默认。
func (c *Config) GeoSiteURL() string {
	return c.geoURL("geosite.dat")
}

// GeoIPURL 由 GeoRepo 推导 geoip-lite.dat 的下载地址；仓库为空时回退到内置默认。
func (c *Config) GeoIPURL() string {
	return c.geoURL("geoip-lite.dat")
}

func (c *Config) geoURL(name string) string {
	if strings.TrimSpace(c.GeoRepo) == "" {
		if name == "geosite.dat" {
			return route.DefaultGeoSiteURL
		}
		return route.DefaultGeoIPURL
	}
	return strings.TrimRight(c.GeoRepo, "/") + "/releases/download/latest/" + name
}

// LoadConfig 读取并解析 config.json。文件缺失时以默认值原子写入一份再返回
// （首次运行即生成模板）；文件存在但解析失败则报错返回，不改动原文件。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if werr := WriteConfig(path, cfg); werr != nil {
				return nil, fmt.Errorf("生成默认配置 %s 失败：%w", path, werr)
			}
			log.Printf("已生成默认配置 %s", path)
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置 %s 失败：%w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败：%w", path, err)
	}
	return cfg, nil
}

// WriteConfig 以"临时文件 + 改名"原子写入配置，读者永远不会看到半写内容。
// tmp 与目标同目录，保证 rename 在同一文件系统内是原子的。
func WriteConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("设置临时文件权限失败：%w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换 %s 失败：%w", path, err)
	}
	return nil
}

// configPollInterval 是配置热重载轮询间隔，与 route 包规则文件热重载一致。
const configPollInterval = 2 * time.Second

// WatchConfig 启动基于轮询的 config.json 热重载：
//   - 每 configPollInterval 检查一次文件的 mtime 与内容 SHA-256，
//     任一变化即重新加载并调用 onReload(cfg, err)（err 为读取/解析错误，
//     此时 cfg 为 nil）
//   - 返回的停止函数用于退出监听 goroutine；可安全重复调用
//   - 文件暂时消失（编辑器原子替换的间隙）不触发回调
//
// 调用方负责在启动监听前完成首次加载，监听只报告"之后的变更"。
func WatchConfig(path string, onReload func(cfg *Config, err error)) (stop func(), err error) {
	baseHash, baseMtime, err := configFileState(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("配置文件 %s 不存在（%w）", path, err)
		}
		return nil, fmt.Errorf("读取配置文件 %s 状态失败：%w", path, err)
	}

	stopCh := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()
		hash, mtime := baseHash, baseMtime
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				nh, nm, err := configFileState(path)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue // 原子替换间隙等瞬态，忽略
					}
					// 永久性读取失败（权限等）：上报一次，避免静默失聪
					log.Printf("⚠ 配置文件 %s 状态读取失败：%v", path, err)
					continue
				}
				if nh == hash && nm == mtime {
					continue
				}
				hash, mtime = nh, nm
				cfg, cerr := LoadConfig(path)
				onReload(cfg, cerr)
			}
		}
	}()

	return func() {
		once.Do(func() { close(stopCh) })
	}, nil
}

// configFileState 返回文件的 mtime 纳秒值与内容 SHA-256 摘要，用于变更检测。
func configFileState(path string) (hash string, mtime int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), info.ModTime().UnixNano(), nil
}
