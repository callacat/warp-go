package main

// Service 是暴露给 Wails 前端的服务层：所有方法可序列化，供 React 前端
// （frontend/src/lib/api.ts）通过生成的 bindings 调用。方法与前端契约一一
// 对应：GetStatus / Start / Stop / GetRules / SaveRules / ReloadRules /
// GetGeo / UpdateGeo / SetSystemProxy / GetSystemProxyEnabled / GetConfig /
// SaveConfig / GetLogs。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"warp/core"
	"warp/registration"
	"warp/route"
)

// Service 持有 core.Server 的惰性实例。GUI 打开时不必立即启动代理：
// 服务方法在需要时才创建/使用 Server。
type Service struct {
	mu           sync.Mutex
	server       *core.Server
	started      bool
	startErr     error // 异步 Start 失败时的错误（GetStatus 展示）
	defaultsInit bool  // InitDefaults 已执行（幂等）
}

// newService 创建服务并注入日志环形缓冲（logs.go）。
func newService() *Service {
	svc := &Service{}
	initLogging()
	// GUI 启动即异步初始化基础文件（rules.txt 模板 + GEO 下载），
	// 不阻塞窗口显示；未注册也能看到默认规则与 GEO 状态。
	go svc.InitDefaults()
	return svc
}

// initLogging 把 log.Printf 输出送入环形缓冲，并去掉标准库 log 的
// 日期/时间前缀（Ldate|Ltime）——环形缓冲已按系统时间生成 HH:MM:SS，
// 双前缀会让前端日志页出现重复时间戳。
func initLogging() {
	log.SetOutput(logWriter{})
	log.SetFlags(0)
}

// server 惰性创建 core.Server（选项来自执行目录 config.json 与默认值）。
// GUI 场景的默认监听地址为 127.0.0.1（安全，不对外暴露）。
func (s *Service) serverInstance() (*core.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return s.server, nil
	}
	// Android 上所有相对运行时路径（config.json/reg.json/rules.txt/geo）
	// 锚定到应用沙箱 getFilesDir()，避免落到只读的 /system/bin 崩溃。
	// dataDir() 依赖 Wails bridge 已初始化（serverInstance 由前端服务调用，
	// 此时 bridge 已就绪）；防御性检查空值。
	if runtime.GOOS == "android" && dataDir() == "" {
		return nil, errors.New("应用沙箱目录未就绪")
	}
	srv := core.New(core.Options{
		ConfigPath: "config.json",
		DataDir:    dataDir(),
	})
	s.server = srv
	return srv, nil
}

// ---------------------------------------------------------------------------
// 状态与生命周期
// ---------------------------------------------------------------------------

// InitDefaults 初始化基础文件（rules.txt 下载/模板 + GEO 下载，不依赖注册）。
// GUI 首次启动时调用；幂等，可安全重复。完成后日志打出"初始化完成"，
// GetStatus.InitDone 置 true（前端据此在初始化完成前禁用启动按钮）。
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
	// 文件已就绪（上一版本的文件都在）时直接标记完成，不再重复下载——
	// 否则每次重开 GUI 都会重新初始化并反复打"初始化完成"，前端又因
	// InitDone 卡住无法启动（v0.5.7 反馈"重开又初始化、无限循环"）。
	if srv.InitDone() {
		s.mu.Lock()
		s.defaultsInit = true
		s.mu.Unlock()
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
	log.Println("✓ 初始化完成（默认配置、默认规则、GEO 数据库已就绪），现在可以启动内核")
}

// GetStatus 返回当前状态快照。
func (s *Service) GetStatus() core.Status {
	s.mu.Lock()
	initDone := s.defaultsInit
	s.mu.Unlock()
	srv, err := s.serverInstance()
	if err != nil {
		if runtime.GOOS == "android" {
			// Android 上 serverInstance 可能因沙箱桥接瞬时失败（Wails bridge
			// 抖动，StoragePath 返回 ""）。此时用缓存的沙箱目录兜底检查
			// reg.json，避免已注册状态被误报为"尚未注册"（页面切换触发）。
			st := core.Status{State: "stopped", InitDone: initDone, LastError: err.Error()}
			if dir := cachedDataDir(); dir != "" {
				if _, serr := os.Stat(filepath.Join(dir, "reg.json")); serr == nil {
					st.Registered = true
				}
			}
			if androidVpnRunning() {
				st.State = "running"
			}
			return st
		}
		return core.Status{State: "stopped", InitDone: initDone, LastError: err.Error()}
	}
	st := srv.Status()
	// InitDone = 本会话已初始化 || 运行时文件已就绪（重启后不再卡初始化）。
	st.InitDone = initDone || srv.InitDone()
	if runtime.GOOS == "android" {
		// Android 上真实隧道状态在 androidRuntime（VpnService 驱动），
		// SOCKS server 永不运行；用 VPN 状态覆盖生命周期字段。
		if androidVpnRunning() {
			st.State = "running"
		} else if st.State != "running" {
			st.State = "stopped"
		}
		if e := androidVpnLastError(); e != "" && st.LastError == "" {
			st.LastError = e
		}
		// 启动时间与分流统计也从真实内核取：Server.kernel 在 Android 永不
		// 启动（startTime 零值、engine nil → Stats 全 0），此前状态页"启动
		// 时间 —"且流量统计卡恒 0（v0.5.22 修复）。androidRuntime.kernel
		// 是 VpnService 驱动的真实内核，其 engine 有实际命中计数。
		if st.StartTime.IsZero() {
			st.StartTime = androidVpnStartTime()
		}
		if k := androidVpnKernel(); k != nil {
			st.Stats = k.Stats()
			st.RulesCount = k.Rules()
		}
	}
	s.mu.Lock()
	if s.startErr != nil && st.LastError == "" {
		st.LastError = s.startErr.Error()
	}
	s.mu.Unlock()
	return st
}

// Start 启动代理（幂等：已在运行则无操作）。
//
// Android 上直接桥接 VpnService（反向 JNI，见 androidbridge.go）；桌面走
// core.Server.Start（阻塞设计，GUI 里必须在 goroutine 中异步启动）。
func (s *Service) Start() error {
	if runtime.GOOS == "android" {
		// 幂等：VPN 已在运行则无操作。
		if androidVpnRunning() {
			log.Println("✓ VPN 已在运行（幂等跳过）")
			return nil
		}
		log.Println("正在启动 VPN（请求系统授权）...")
		if err := androidRequestVpnStart(); err != nil {
			return err
		}
		return nil
	}

	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		return nil
	}

	// 注意：不能在持有 s.mu 时调用 serverInstance()——其内部再次加锁
	// （sync.Mutex 不可重入），会自死锁：GUI 服务线程永久阻塞，之后所有
	// 服务调用（GetStatus/GetRules/GetGeo…）都卡在锁上，表现为"点击启动
	// 卡死，其他页全部无法显示"。与 InitDefaults 的注释同源。
	srv, err := s.serverInstance()
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
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
	if runtime.GOOS == "android" {
		log.Println("正在停止 VPN...")
		if err := androidRequestVpnStop(); err != nil {
			return err
		}
		return nil
	}

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
	if runtime.GOOS == "android" {
		return androidVpnRunning()
	}
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
	// Android 上 serverInstance 可能因桥接抖动失败；注销不依赖内核，
	// 用缓存的沙箱目录兜底直接调 DeleteRegistration（API 注销 + 删文件）。
	if runtime.GOOS == "android" {
		if dir := cachedDataDir(); dir != "" {
			return registration.DeleteRegistration(filepath.Join(dir, "reg.json"))
		}
	}
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
	// 触发引擎重载（规则文件热重载会自行发现；显式调用保证立即生效）。
	_ = srv.ReloadRules()
	return nil
}

// ReloadRules 从磁盘重新加载规则。
func (s *Service) ReloadRules() error {
	// Android 上分流引擎挂在 androidRuntime.kernel（VpnService 驱动的
	// core.Kernel），core.Server.kernel 在 Android 永不初始化（SOCKS 内核
	// 不跑）——若走 Server.ReloadRules 必然报"分流引擎未初始化"且规则不
	// 生效（v0.5.12 真机反馈）。路由到 androidRuntime.kernel.ReloadRules。
	if runtime.GOOS == "android" {
		return androidReloadRules()
	}
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
	if st.Config != nil && st.Config.GeoRepo != "" {
		info.BaseURL = strings.TrimRight(st.Config.GeoRepo, "/") + "/releases/download/latest"
	} else {
		info.BaseURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"
	}

	var latestMtime time.Time
	if fi, err := os.Stat(info.GeositePath); err == nil {
		info.GeositeUpdated = fi.ModTime().Format("2006-01-02 15:04")
		if fi.ModTime().After(latestMtime) {
			latestMtime = fi.ModTime()
		}
	}
	if fi, err := os.Stat(info.GeoIPPath); err == nil {
		info.GeoIPUpdated = fi.ModTime().Format("2006-01-02 15:04")
		if fi.ModTime().After(latestMtime) {
			latestMtime = fi.ModTime()
		}
	}
	// LastChecked 用 GEO 文件的最新 mtime 作为"上次检查时间"代理值，
	// 而非 time.Now()——后者每次调用都返回当前时间，没有信息量。
	if !latestMtime.IsZero() {
		info.LastChecked = latestMtime.Format("2006-01-02 15:04")
	}
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
	if runtime.GOOS == "android" {
		// Android 上系统代理无意义：VpnService 的 TUN 已接管全部流量，
		// 没有 gsettings/networksetup/注册表可设。明确报错而非静默成功，
		// 前端显示提示（此前静默成功但用户看到"系统代理未生效"）。
		return errors.New("Android 由 VPN 接管全部流量，无需设置系统代理")
	}

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

// SaveConfig 校验并保存配置；写入即生效（内存快照同步），无需重启。
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

// ClearLogs 清空日志环形缓冲（日志页"清空"按钮调用）。
// 此前前端只清了本地 state，后端缓冲不变——轮询下一帧旧日志又回来。
func (s *Service) ClearLogs() {
	ringLog.Clear()
}

// GetVersion 返回构建版本（前端设置页展示）。版本号经 ldflags 注入，
// 与 release tag 同源；本地构建返回 "dev"。
func (s *Service) GetVersion() string {
	return VersionString()
}

// CheckUpdate 查询 GitHub Releases 最新版本并返回更新信息。
// 网络失败返回错误（前端显示"检查失败"，非致命）；dev 版本不参与比较
// （永远提示可更新，引导用户装正式版）。
func (s *Service) CheckUpdate() (*core.UpdateInfo, error) {
	cur := strings.TrimPrefix(VersionString(), "v")
	if cur == "dev" {
		cur = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return core.CheckUpdate(ctx, cur)
}

// OpenExternalBrowser 用系统浏览器打开 URL（更新下载页等）。
// 桌面端走 Wails BrowserManager（调系统默认浏览器）；Android 走反向 JNI 桥
// 跳第三方浏览器——WebView 内 target=_blank 会被应用内捕获，GitHub 下载页
// 在 WebView 里体验差/登录墙（v0.5.11 反馈）。
func (s *Service) OpenExternalBrowser(url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("链接为空")
	}
	if runtime.GOOS == "android" {
		return androidOpenExternalBrowser(url)
	}
	app := application.Get()
	if app == nil || app.Browser == nil {
		return errors.New("应用未初始化，无法打开浏览器")
	}
	return app.Browser.OpenURL(url)
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
