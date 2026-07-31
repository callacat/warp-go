package main

// Service 是暴露给 Wails 前端的服务层：所有方法可序列化，供 React 前端
// （frontend/src/lib/api.ts）通过生成的 bindings 调用。方法与前端契约一一
// 对应：GetStatus / Start / Stop / GetRules / SaveRules / ReloadRules /
// GetGeo / UpdateGeo / SetSystemProxy / GetSystemProxyEnabled / GetConfig /
// SaveConfig / GetLogs。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"warp/core"
	"warp/route"
)

// Service 持有 core.Server 的惰性实例。GUI 打开时不必立即启动代理：
// 服务方法在需要时才创建/使用 Server。
type Service struct {
	mu           sync.Mutex
	server       *core.Server
	started      bool
	startErr     error // 异步 Start 失败时的错误（GetStatus 展示）
	defaultsInit bool // InitDefaults 已执行（幂等）
}

// newService 创建服务并注入日志环形缓冲（logs.go）。
func newService() *Service {
	svc := &Service{}
	log.SetOutput(logWriter{})
	// GUI 启动即异步初始化基础文件（rules.txt 模板 + GEO 下载），
	// 不阻塞窗口显示；未注册也能看到默认规则与 GEO 状态。
	go svc.InitDefaults()
	return svc
}

// server 惰性创建 core.Server（选项来自执行目录 config.json 与默认值）。
// GUI 场景的默认监听地址为 127.0.0.1（安全，不对外暴露）。
func (s *Service) serverInstance() (*core.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return s.server, nil
	}
	srv := core.New(core.Options{
		ConfigPath: "config.json",
	})
	s.server = srv
	return srv, nil
}

// ---------------------------------------------------------------------------
// 状态与生命周期
// ---------------------------------------------------------------------------

// InitDefaults 初始化基础文件（rules.txt 下载/模板 + GEO 下载，不依赖注册）。
// GUI 首次启动时调用；幂等，可安全重复。
func (s *Service) InitDefaults() {
	s.mu.Lock()
	if s.defaultsInit {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	// 注意：不能在持有 s.mu 时调用 serverInstance()（其内部再次加锁，
	// 非重入互斥锁会永久死锁，导致 GUI 全部服务调用阻塞）。
	srv, err := s.serverInstance()
	if err != nil {
		log.Printf("⚠ 初始化失败：%v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := srv.InitDefaults(ctx); err != nil {
		log.Printf("⚠ 初始化基础文件失败：%v", err)
		return
	}
	s.mu.Lock()
	s.defaultsInit = true
	s.mu.Unlock()
}

// GetStatus 返回当前状态快照。
func (s *Service) GetStatus() core.Status {
	srv, err := s.serverInstance()
	if err != nil {
		return core.Status{State: "stopped", LastError: err.Error()}
	}
	st := srv.Status()
	s.mu.Lock()
	if s.startErr != nil && st.LastError == "" {
		st.LastError = s.startErr.Error()
	}
	s.mu.Unlock()
	return st
}

// Start 启动代理（幂等：已在运行则无操作）。
//
// core.Server.Start 是阻塞设计（CLI 主流程用）；GUI 里必须在 goroutine 中
// 异步启动，否则会卡死 Wails 服务线程导致整个界面冻结。
func (s *Service) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	srv, err := s.serverInstance()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	s.mu.Lock()
	s.started = true
	s.startErr = nil
	s.mu.Unlock()

	go func() {
		// 阻塞直到 Stop/退出；错误记录到 startErr 供 GetStatus 展示。
		err := srv.Start(context.Background())
		s.mu.Lock()
		if err != nil {
			s.startErr = err
			log.Printf("代理启动失败：%v", err)
		}
		s.started = false
		s.mu.Unlock()
	}()
	return nil
}

// Stop 停止代理（幂等）。
func (s *Service) Stop() error {
	s.mu.Lock()
	srv := s.server
	started := s.started
	s.started = false
	s.mu.Unlock()

	if srv == nil || !started {
		return nil
	}
	return srv.Stop()
}

// IsRunning 报告代理是否运行中。
func (s *Service) IsRunning() bool {
	srv, err := s.serverInstance()
	if err != nil {
		return false
	}
	return srv.Status().State == "running"
}

// Register 执行 WARP 注册（幂等；已有注册则报告 existing=true）。
func (s *Service) Register() (existing bool, id string, err error) {
	srv, err := s.serverInstance()
	if err != nil {
		return false, "", err
	}
	return srv.Register()
}

// Deregister 注销并删除本地注册。
func (s *Service) Deregister() error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	return srv.Deregister()
}

// ---------------------------------------------------------------------------
// 路由规则
// ---------------------------------------------------------------------------

// GetRules 返回 rules.txt 当前完整文本（前端逐行展示/编辑）。
func (s *Service) GetRules() (string, error) {
	srv, err := s.serverInstance()
	if err != nil {
		return "", err
	}
	st := srv.Status()
	rulesPath := "rules.txt"
	if st.Config != nil && st.Config.RulesPath != "" {
		rulesPath = st.Config.RulesPath
	}
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		return "", fmt.Errorf("读取规则文件失败：%w", err)
	}
	return string(data), nil
}

// SaveRules 校验并写回 rules.txt（引擎热重载自动生效）。
func (s *Service) SaveRules(rulesText string) error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	st := srv.Status()
	rulesPath := "rules.txt"
	if st.Config != nil && st.Config.RulesPath != "" {
		rulesPath = st.Config.RulesPath
	}
	// 先校验语法，非法内容拒绝写盘。
	if _, err := route.ParseRules(rulesText); err != nil {
		return fmt.Errorf("规则语法错误：%w", err)
	}
	if err := atomicWriteFile(rulesPath, []byte(rulesText)); err != nil {
		return fmt.Errorf("写入规则文件失败：%w", err)
	}
	// 触发引擎重载（文件变更热重载会自行发现；显式调用保证立即生效）。
	_ = srv.ReloadRules()
	return nil
}

// ReloadRules 从磁盘重新加载规则。
func (s *Service) ReloadRules() error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	return srv.ReloadRules()
}

// ---------------------------------------------------------------------------
// GEO 数据库
// ---------------------------------------------------------------------------

// GeoInfo 是 GEO 页展示的数据快照。
type GeoInfo struct {
	GeositePath    string `json:"geosite_path"`
	GeoIPPath      string `json:"geoip_path"`
	GeositeUpdated string `json:"geosite_updated,omitempty"`
	GeoIPUpdated   string `json:"geoip_updated,omitempty"`
	Repository     string `json:"repository"`
	BaseURL        string `json:"base_url"`
	AutoUpdateDays int    `json:"auto_update_days"`
	LastChecked    string `json:"last_checked,omitempty"`
}

// GetGeo 返回 GEO 数据库状态。
func (s *Service) GetGeo() (GeoInfo, error) {
	srv, err := s.serverInstance()
	if err != nil {
		return GeoInfo{}, err
	}
	st := srv.Status()

	info := GeoInfo{
		Repository:     "https://github.com/MetaCubeX/meta-rules-dat",
		AutoUpdateDays: 7,
	}
	if st.Config != nil {
		info.Repository = st.Config.GeoRepo
		info.AutoUpdateDays = st.Config.GeoAutoUpdateDays
	}
	geoDir := "geo"
	if st.Config != nil && st.Config.GeoDir != "" {
		geoDir = st.Config.GeoDir
	}
	info.GeositePath = filepath.Join(geoDir, "geosite.dat")
	info.GeoIPPath = filepath.Join(geoDir, "geoip-lite.dat")
	info.BaseURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"

	if fi, err := os.Stat(info.GeositePath); err == nil {
		info.GeositeUpdated = fi.ModTime().Format("2006-01-02 15:04")
	}
	if fi, err := os.Stat(info.GeoIPPath); err == nil {
		info.GeoIPUpdated = fi.ModTime().Format("2006-01-02 15:04")
	}
	info.LastChecked = time.Now().Format("2006-01-02 15:04")
	return info, nil
}

// UpdateGeoResult 是手动更新 GEO 的结果。
type UpdateGeoResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// UpdateGeo 立即更新 GEO 数据库。
func (s *Service) UpdateGeo() UpdateGeoResult {
	srv, err := s.serverInstance()
	if err != nil {
		return UpdateGeoResult{OK: false, Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	updated, err := srv.UpdateGeo(ctx)
	if err != nil {
		return UpdateGeoResult{OK: false, Message: "更新失败：" + err.Error()}
	}
	if updated {
		return UpdateGeoResult{OK: true, Message: "GEO 数据已更新"}
	}
	return UpdateGeoResult{OK: true, Message: "GEO 数据已是最新"}
}

// ---------------------------------------------------------------------------
// 系统代理
// ---------------------------------------------------------------------------

// SetSystemProxy 开启/关闭系统代理（指向当前监听地址）。
// 开启且内核未运行时自动启动代理内核（用户需求：开启系统代理即启动
// warp-go）。未注册时 Start 会报错并记录到 startErr/日志，不会挂起。
func (s *Service) SetSystemProxy(enabled bool) error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}

	if enabled && !s.IsRunning() {
		if err := s.Start(); err != nil {
			return fmt.Errorf("自动启动内核失败：%w", err)
		}
		// Start 是异步的：等待内核进入 running 再设置系统代理，避免
		// 指向尚未监听的端口。最多等 10s，超时仍返回（Start 的启动错误
		// 会经 startErr 展示在状态页）。
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			st := srv.Status()
			if st.State == "running" {
				return srv.SetSystemProxy(true)
			}
			if st.LastError != "" && st.State != "starting" {
				return fmt.Errorf("内核启动失败：%s", st.LastError)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return nil // 超时：内核仍在启动，配置意向已记录（Start 会自动应用）
	}
	return srv.SetSystemProxy(enabled)
}

// GetSystemProxyEnabled 报告系统代理当前是否指向本程序。
func (s *Service) GetSystemProxyEnabled() bool {
	srv, err := s.serverInstance()
	if err != nil {
		return false
	}
	return srv.Status().SysProxyOn
}

// ScanEdges 扫描 WARP 边缘，返回最优端点列表（GUI "扫描最优边缘"按钮）。
func (s *Service) ScanEdges() ([]string, error) {
	return s.scanEdges("4")
}

// ScanEdgesV4 扫描 IPv4 边缘。
func (s *Service) ScanEdgesV4() ([]string, error) {
	return s.scanEdges("4")
}

// ScanEdgesV6 扫描 IPv6 边缘。
func (s *Service) ScanEdgesV6() ([]string, error) {
	return s.scanEdges("6")
}

func (s *Service) scanEdges(ipMode string) ([]string, error) {
	srv, err := s.serverInstance()
	if err != nil {
		return nil, err
	}
	// 用临时 Options 覆盖 EdgeIP，扫描指定地址族。
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return srv.ScanEdgesFamily(ctx, ipMode)
}

// ApplyEdge 应用扫描选出的最优边缘地址（写入 config.json，下次启动生效）。
func (s *Service) ApplyEdge(addr string) error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	return srv.SetEdgeAddr(addr)
}

// SetAutostart 开启/关闭开机自启。
func (s *Service) SetAutostart(enabled bool) error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	return srv.SetAutostart(enabled)
}

// GetAutostartEnabled 报告开机自启状态。
func (s *Service) GetAutostartEnabled() bool {
	srv, err := s.serverInstance()
	if err != nil {
		return false
	}
	return srv.AutostartEnabled()
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// GetConfig 返回当前生效配置（前端设置页表单数据源）。
func (s *Service) GetConfig() core.Config {
	srv, err := s.serverInstance()
	if err != nil {
		return *core.DefaultConfig()
	}
	st := srv.Status()
	if st.Config != nil {
		return *st.Config
	}
	return *core.DefaultConfig()
}

// SaveConfig 校验并保存配置；config.json 热重载自动应用。
func (s *Service) SaveConfig(cfg core.Config) error {
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}
	return srv.SaveConfig(&cfg)
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// GetLogs 返回最近 limit 条日志（环形缓冲，logs.go）。
func (s *Service) GetLogs(limit int) []LogEntry {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return ringLog.Snapshot(limit)
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// atomicWriteFile 先写临时文件再原子改名，避免半写文件被读取。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".warp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// logWriter 把 log.Printf 的输出同时送入环形缓冲（logs.go）。
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	ringLog.Append(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
