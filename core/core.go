package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"warp/autostart"
	"warp/proxy"
	"warp/registration"
	"warp/route"
	"warp/scanner"
)

// defaultStateFile holds the registration: keys, token, edge endpoint and the
// edge public key that the connection is pinned to.
const defaultStateFile = "reg.json"

// geoUpdateOneShotTimeout bounds a single GEO update run. Each file download
// already carries its own 5-minute request timeout, so this only caps the
// whole operation.
const geoUpdateOneShotTimeout = 10 * time.Minute

// edgeLookupTimeout bounds the bootstrap name lookup for an -ip hostname.
const edgeLookupTimeout = 10 * time.Second

// ErrNoRegistration 表示启动所需的本机注册信息缺失（reg.json 不存在）。
var ErrNoRegistration = errors.New("没有注册信息，请先执行 warp -reg")

// ErrAlreadyRunning 表示 Start 被重复调用。
var ErrAlreadyRunning = errors.New("server 已在运行")

// state 是 Server 的生命周期状态机。
type state int

const (
	stateStopped state = iota
	stateStarting
	stateRunning
	stateStopping
)

// Options 配置一个 Server。零值由 New 填充默认值；旗标对应的字段语义与
// CLI 一致（优先级：Options > config.json > 默认值）。
type Options struct {
	// ConfigPath 是 config.json 路径（默认 "config.json"，缺失时自动生成
	// 默认配置模板）。config.json 负责 rules_path / geo_dir / 系统代理开关
	// 等运行时配置，文件变更热重载。
	ConfigPath string

	// StateFile 是注册信息文件路径（默认 "reg.json"）。
	StateFile string

	// ListenAddr 覆盖 config.json 的 listen_addr（CLI -l）。
	ListenAddr string

	// Username / Password 同时非空时启用认证（CLI -user / -pass）。
	Username string
	Password string

	// EdgeIP 选择连接哪个边缘："4" / "6" 取注册信息中对应地址族，或显式
	// host:port（CLI -ip）。
	EdgeIP string

	// RulesPath 覆盖 config.json 的 rules_path（CLI -route）。
	RulesPath string

	// SysProxy 覆盖 config.json 的 enable_system_proxy（CLI -sysproxy）。
	// nil 表示不覆盖（按 config.json）；非 nil 时强制启用/禁用。
	SysProxy *bool

	// Scan 启动前扫描 WARP 边缘全段并选用最低延迟的端点（CLI -scan 族）。
	Scan            bool
	ScanCIDR        string
	ScanPorts       string
	ScanConcurrency int
	ScanTimeout     time.Duration
	ScanPerProbe    time.Duration
	ScanTop         int
}

// Server 是 CLI 与 GUI 共用的可复用核心：持有配置、注册信息、分流引擎、
// mixed 代理与 MASQUE 隧道，提供 Start/Stop 生命周期与可序列化状态。
type Server struct {
	opts Options

	// mu 保护全部可变字段（生命周期状态、资源引用、状态快照）。Start/Stop
	// 不持有跨阻塞点的锁：Stop 只置位 + 关 stopCh，由 Start 的 select 醒来
	// 执行关停，避免"Stop 等 Start 的锁、Start 等 Stop 的 channel"死锁。
	mu sync.Mutex
	st state

	stopRequested bool // Stop 已请求（Start 过渡到 running 前到达时立即关停）
	stopCh        chan struct{}

	cfg             *Config
	reg             *registration.Registration
	kernel          *Kernel
	server          *proxy.Server
	listenAddr      string
	edgeAddrs       []string
	startTime       time.Time
	lastError       string
	geoCancel       context.CancelFunc
	stopWatch       func()
	sysProxyEnabled atomic.Bool
}

// New 创建 Server 并填充 Options 默认值。默认 StateFile 为 reg.json、
// EdgeIP 为 "4"；扫描参数沿用 CLI 默认（45s 总超时、3s 单探针、top-4）。
//
// 所有运行时文件路径（config.json / reg.json / rules.txt / geo）锚定到
// 可执行文件所在目录：GUI 双击启动时工作目录可能是用户主目录，相对路径
// 会让文件散落各处。绝对路径保持不变。
func New(opts Options) *Server {
	if opts.ConfigPath == "" {
		opts.ConfigPath = "config.json"
	}
	if opts.StateFile == "" {
		opts.StateFile = defaultStateFile
	}
	if opts.EdgeIP == "" {
		opts.EdgeIP = "4"
	}
	if opts.ScanTimeout == 0 {
		opts.ScanTimeout = 45 * time.Second
	}
	if opts.ScanPerProbe == 0 {
		opts.ScanPerProbe = 3 * time.Second
	}
	if opts.ScanTop == 0 {
		opts.ScanTop = 4
	}
	opts.ConfigPath = resolveExecPath(opts.ConfigPath)
	opts.StateFile = resolveExecPath(opts.StateFile)
	return &Server{opts: opts}
}

// resolveExecPath 把相对路径解析为数据目录下的绝对路径：
//   - 优先可执行文件所在目录（便携部署：exe 放哪数据就在哪）
//   - 可执行目录不可写（如 Windows Program Files）时回退用户配置目录
//     （Windows %APPDATA%/warp-go、macOS ~/Library/Application Support/warp-go、
//     Linux ~/.config/warp-go）
//
// 已是绝对路径或空串时原样返回。os.Executable 失败（罕见）时回退到
// 当前工作目录，保证程序仍能启动。
func resolveExecPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return p
	}
	exeDir := filepath.Dir(exe)
	if dirWritable(exeDir) {
		return filepath.Join(exeDir, p)
	}
	if cfgDir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfgDir, "warp-go", p)
	}
	return filepath.Join(exeDir, p)
}

// dirWritable 检查目录是否可写（用临时文件探测；失败按不可写处理）。
func dirWritable(dir string) bool {
	tmp, err := os.CreateTemp(dir, ".warp-write-test-*")
	if err != nil {
		return false
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	return true
}

// ensureConfig 返回生效中的配置：Start 后返回启动时加载的实例，否则按
// Options.ConfigPath 现加载（CLI -geo-update 等无需 Start 的路径也用）。
// RulesPath 覆盖在此统一应用。
func (s *Server) ensureConfig() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		return s.cfg, nil
	}
	cfg, err := LoadConfig(s.opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.opts.RulesPath) != "" {
		cfg.RulesPath = s.opts.RulesPath
	}
	// config.json 内的相对路径同样锚定到可执行目录。
	cfg.RulesPath = resolveExecPath(cfg.RulesPath)
	cfg.GeoDir = resolveExecPath(cfg.GeoDir)
	s.cfg = cfg
	return cfg, nil
}

// Start 启动代理并阻塞，直到 ctx 被取消、Stop 被调用或代理出现致命错误。
//
// 启动序列（与旧 CLI main 一致）：加载配置与注册信息 → 解析边缘候选 →
// 公钥固定 TLS → 可选边缘扫描 → 建立 MASQUE 连接 → 分流引擎 →
// mixed 代理监听 → 系统代理 → GEO 自动更新与配置热重载协程 → 阻塞等待。
// 任一步失败都会清理已建立的资源后返回错误。
//
// 注意：tunnel.NewMasqueClient 内部重试直到连通，不响应 ctx 取消，因此
// 启动期间的取消请求会等到连接建立后才生效（与旧行为一致）。
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.st != stateStopped {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.stopRequested = false
	s.st = stateStarting
	s.lastError = ""
	s.mu.Unlock()

	started := false
	defer func() {
		if !started {
			s.shutdown()
		}
	}()

	// 配置（优先级：Options > config.json > 默认值）。
	cfg, err := s.ensureConfig()
	if err != nil {
		return fmt.Errorf("加载配置失败：%w", err)
	}

	// 启动从不注册：创建账号是需要明确表达的动作（见 CLI -reg）。
	regData, err := registration.Load(s.opts.StateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s 中没有注册信息：%w", s.opts.StateFile, ErrNoRegistration)
		}
		return fmt.Errorf("无法读取注册文件 %s：%w", s.opts.StateFile, err)
	}
	log.Printf("✓ 已注册：id=%s", regData.ID)
	s.mu.Lock()
	s.reg = regData
	s.mu.Unlock()

	// 边缘候选：-ip 取 "4"/"6" 时按注册信息展开端口列表；显式 host:port
	// 走系统解析器（此时隧道尚未建立，in-tunnel DoH 不可用）。
	var edgeAddrs []string
	if strings.TrimSpace(cfg.EdgeAddr) != "" && s.opts.EdgeIP == "" {
		// 扫描应用的边缘地址优先（config.json edge_addr）。
		if edgeAddrs, err = resolveEdge(cfg.EdgeAddr); err != nil {
			return fmt.Errorf("edge_addr %q 无法解析：%w", cfg.EdgeAddr, err)
		}
		log.Printf("WARP 代理启动中（边缘=已应用扫描结果 %s，mixed=%s）",
			cfg.EdgeAddr, s.opts.ListenAddr)
	} else {
		switch s.opts.EdgeIP {
		case "4", "6":
			endpointHost, other := regData.EndpointV4, "6"
			if s.opts.EdgeIP == "6" {
				endpointHost, other = regData.EndpointV6, "4"
			}
			if endpointHost == "" {
				return fmt.Errorf("注册信息中没有 IPv%s 边缘地址。"+
					"可改用 -ip %s，或依次执行 -del 与 -reg 重新注册", s.opts.EdgeIP, other)
			}
			ports := regData.EndpointPorts
			if len(ports) == 0 {
				ports = []int{443}
			}
			for _, p := range ports {
				edgeAddrs = append(edgeAddrs, net.JoinHostPort(endpointHost, strconv.Itoa(p)))
			}
			log.Printf("WARP 代理启动中（边缘=IPv%s %s 端口=%v，mixed=%s）",
				s.opts.EdgeIP, endpointHost, ports, s.opts.ListenAddr)
		default:
			if edgeAddrs, err = resolveEdge(s.opts.EdgeIP); err != nil {
				return fmt.Errorf("-ip %q 既不是 4 或 6，也不是可用地址：%w", s.opts.EdgeIP, err)
			}
			log.Printf("WARP 代理启动中（边缘=%s → %v，mixed=%s）", s.opts.EdgeIP, edgeAddrs, s.opts.ListenAddr)
		}
	}

	// 公钥固定 + TLS 配置（与官方 warp-svc 对齐，见逆向文档）。
	verifyEdge, err := regData.PeerPublicKeyVerifier()
	if err != nil {
		return fmt.Errorf("边缘公钥固定初始化失败：%w", err)
	}
	tlsConfig := &tls.Config{
		ServerName:            "consumer-masque-proxy.cloudflareclient.com",
		NextProtos:            []string{"h3"},
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{regData.ClientCert},
		VerifyPeerCertificate: verifyEdge,
		// warp-svc 只提供 NIST 曲线，Go 默认先发 X25519 会引来一次额外的
		// HelloRetryRequest 往返。
		CurvePreferences: []tls.CurveID{tls.CurveP256, tls.CurveP384, tls.CurveP521},
	}
	if verifyEdge != nil {
		log.Println("✓ 边缘公钥固定已启用")
	} else {
		log.Println("⚠ 注册信息中没有边缘公钥，公钥固定已禁用（请重新执行 -reg）")
	}

	// 可选：启动前边缘延迟扫描（-ip 为显式端点时忽略，显式端点优先）。
	if s.opts.Scan {
		switch s.opts.EdgeIP {
		case "4", "6":
			edgeAddrs = runEndpointScan(s.opts.EdgeIP == "6", edgeAddrs, regData, tlsConfig,
				s.opts.ScanCIDR, s.opts.ScanPorts, s.opts.ScanConcurrency,
				s.opts.ScanTimeout, s.opts.ScanPerProbe, s.opts.ScanTop)
		default:
			log.Printf("⚠ -ip %q 指定了显式端点，-scan 被忽略（显式端点优于自动优选）", s.opts.EdgeIP)
		}
	}

	// Kernel：MASQUE 隧道 + 分流引擎（NewKernel 内部先建隧道后建引擎，
	// 与旧时序一致；隧道重试到连通，引擎自动初始化默认 rules.txt 模板、
	// 加载规则与 GEO 库——缺失时降级 rules-only、启动规则文件热重载）。
	kernel, err := NewKernel(cfg, regData, edgeAddrs, tlsConfig)
	if err != nil {
		return fmt.Errorf("Kernel 初始化失败：%w", err)
	}
	log.Println("✓ MASQUE 连接已建立")
	log.Printf("✓ 分流引擎就绪（规则=%s，%d 条；GEO=%s）", cfg.RulesPath, len(kernel.engine.get().Rules()), cfg.GeoDir)

	// mixed 代理：同一端口按首字节嗅探 HTTP 与 SOCKS5。Router 命中
	// "direct" 时本地直连，命中 "proxy"（或未配置规则）时走 WARP 隧道。
	listenAddr := s.opts.ListenAddr
	if listenAddr == "" {
		listenAddr = cfg.ListenAddr
	}
	server := proxy.NewServer(proxy.Config{
		ListenAddr: listenAddr,
		Username:   s.opts.Username,
		Password:   s.opts.Password,
		AllowUDP:   cfg.AllowUDP,
		Router:     kernel.Route,
		TunnelDial: kernel.DialTunnel,
	})

	// 系统代理（Options.SysProxy 优先于 config.json 的 enable_system_proxy）。
	if sysProxy := cfg.EnableSystemProxy; s.opts.SysProxy == nil || *s.opts.SysProxy == sysProxy {
		if sysProxy {
			if err := setSystemProxy(listenAddr, true); err != nil {
				log.Printf("⚠ 设置系统代理失败（代理继续运行）：%v", err)
			} else {
				s.sysProxyEnabled.Store(true)
				log.Printf("✓ 系统代理已指向 %s", listenAddr)
			}
		}
	} else if *s.opts.SysProxy {
		if err := setSystemProxy(listenAddr, true); err != nil {
			log.Printf("⚠ 设置系统代理失败（代理继续运行）：%v", err)
		} else {
			s.sysProxyEnabled.Store(true)
			log.Printf("✓ 系统代理已指向 %s", listenAddr)
		}
	}

	// GEO 自动更新：数据缺失时启动即补一份，之后按 GeoAutoUpdateDays
	// 周期更新（0 表示关闭）。失败只打 warning，下个周期重试。
	geoCtx, geoCancel := context.WithCancel(context.Background())
	defer geoCancel() // 提前返回路径（如热重载启动失败）不泄漏 context
	if cfg.GeoAutoUpdateDays > 0 {
		interval := time.Duration(cfg.GeoAutoUpdateDays) * 24 * time.Hour
		go func() {
			if !geoDataPresent(cfg.GeoDir) {
				log.Println("GEO 数据缺失，立即更新...")
				s.geoUpdateOnce(geoCtx)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-geoCtx.Done():
					return
				case <-ticker.C:
					s.geoUpdateOnce(geoCtx)
				}
			}
		}()
	}

	// config.json 热重载：rules/geo 路径与系统代理开关即时生效，其余需重启。
	stopWatch, err := WatchConfig(s.opts.ConfigPath, func(nc *Config, rerr error) {
		if rerr != nil {
			log.Printf("⚠ 配置热重载失败（保持原配置生效）：%v", rerr)
			return
		}
		s.applyConfigReload(cfg, nc)
	})
	if err != nil {
		return fmt.Errorf("启动配置热重载失败：%w", err)
	}

	// 启动 mixed 代理（监听与 Accept 循环在 proxy 包内）。
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	authInfo := ""
	if s.opts.Username != "" && s.opts.Password != "" {
		authInfo = fmt.Sprintf("（认证用户：%s）", s.opts.Username)
	}
	log.Printf("mixed 代理监听于 %s%s", listenAddr, authInfo)
	if cfg.AllowUDP {
		log.Println("UDP ASSOCIATE 已启用 —— 数据报从本机直接发出，不经过 WARP 隧道")
	}

	s.mu.Lock()
	s.cfg = cfg
	s.reg = regData
	s.kernel = kernel
	s.server = server
	s.listenAddr = listenAddr
	s.edgeAddrs = edgeAddrs
	s.startTime = time.Now()
	s.geoCancel = geoCancel
	s.stopWatch = stopWatch
	if s.stopRequested {
		s.st = stateStopping
		s.mu.Unlock()
		s.shutdown()
		return nil
	}
	s.st = stateRunning
	s.mu.Unlock()
	started = true

	// 阻塞：等待代理启动错误（端口占用等致命）、ctx 取消或 Stop()。
	select {
	case err := <-serverErr:
		// ListenAndServe 仅在监听失败时返回错误；正常关停返回 nil。
		s.setLastError(err)
		s.shutdown()
		if err != nil {
			return fmt.Errorf("代理退出：%w", err)
		}
		return nil
	case <-ctx.Done():
		log.Println("正在关闭...")
		s.shutdown()
		return nil
	case <-s.stopSignal():
		log.Println("正在关闭...")
		s.shutdown()
		return nil
	}
}

// stopSignal 返回一个在 Stop() 被调用时关闭的 channel。
func (s *Server) stopSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopRequested {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	ch := make(chan struct{})
	s.stopCh = ch
	return ch
}

// Stop 请求停止运行中的 Server（幂等，立即返回）。Stop 不持有阻塞锁：
// 若 Start 已进入 select，关闭 stopCh 唤醒它执行关停；若 Stop 在启动
// 序列完成前到达，Start 会在过渡到 running 前自行关停。关停进度可通过
// Status() 轮询（GUI 需要非阻塞的停止请求）。
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.st == stateStopped {
		s.mu.Unlock()
		return nil
	}
	s.stopRequested = true
	s.st = stateStopping
	ch := s.stopCh
	s.stopCh = nil
	s.mu.Unlock()

	if ch != nil {
		close(ch)
	}
	return nil
}

// setLastError 记录最近一次致命错误（Status 展示用）。
func (s *Server) setLastError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastError = err.Error()
	}
}

// shutdown 按序拆除全部资源（幂等，可重复调用）：
// 停 GEO 自动更新协程 → 停监听与新连接 → 撤系统代理 → 停配置热重载 →
// 关分流引擎 → 拆 MASQUE 隧道。
func (s *Server) shutdown() {
	s.mu.Lock()
	if s.st == stateStopped {
		s.mu.Unlock()
		return
	}
	geoCancel := s.geoCancel
	server := s.server
	stopWatch := s.stopWatch
	kernel := s.kernel
	listenAddr := s.listenAddr
	sysProxyEnabled := s.sysProxyEnabled.Load()

	s.server = nil
	s.kernel = nil
	s.geoCancel = nil
	s.stopWatch = nil
	s.st = stateStopped
	s.mu.Unlock()

	if geoCancel != nil {
		geoCancel()
	}
	if server != nil {
		_ = server.Close()
	}
	if sysProxyEnabled {
		if err := setSystemProxy(listenAddr, false); err != nil {
			log.Printf("⚠ 清除系统代理失败：%v", err)
		} else {
			log.Println("✓ 系统代理已清除")
		}
	}
	if stopWatch != nil {
		stopWatch()
	}
	if kernel != nil {
		_ = kernel.Close()
	}
	log.Println("已退出")
}

// Status 返回当前状态的序列化快照（每次调用生成新拷贝，可安全跨协程与
// Wails 绑定边界传递）。
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	names := map[state]string{
		stateStopped:  "stopped",
		stateStarting: "starting",
		stateRunning:  "running",
		stateStopping: "stopping",
	}
	st := Status{
		State:      names[s.st],
		ListenAddr: s.listenAddr,
		EdgeAddrs:  s.edgeAddrs,
		GeoReady:   s.cfg != nil && geoDataPresent(s.cfg.GeoDir),
		SysProxyOn: s.sysProxyEnabled.Load(),
		Registered: registrationFileExists(s.opts.StateFile),
		StartTime:  s.startTime,
		LastError:  s.lastError,
	}
	if s.cfg != nil {
		c := *s.cfg
		st.Config = &c
	}
	if s.kernel != nil {
		if e := s.kernel.engine.get(); e != nil {
			st.RulesCount = len(e.Rules())
			st.Stats = e.Stats()
		}
	}
	if s.reg != nil {
		st.Registration = registrationView(s.reg)
	}
	return st
}

// UpdateGeo 立即按配置仓库更新 GEO 数据（SHA-1 去重，无变更零开销）。
// 运行中会重建分流引擎热加载新库；未运行时只更新数据文件（供 CLI
// -geo-update 与 GUI "立即更新"按钮复用）。返回是否实际有内容变更。
func (s *Server) UpdateGeo(ctx context.Context) (bool, error) {
	cfg, err := s.ensureConfig()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	k := s.kernel
	s.mu.Unlock()

	updated, err := route.UpdateGeoData(ctx, cfg.GeoDir, cfg.GeoSiteURL(), cfg.GeoIPURL())
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}
	if k != nil {
		ne, err := route.NewEngine(cfg.RulesPath, cfg.GeoDir)
		if err != nil {
			return true, fmt.Errorf("GEO 数据已更新，但重建引擎失败（重启后生效）：%w", err)
		}
		k.engine.swap(ne)
	}
	return true, nil
}

// InitDefaults 初始化不依赖注册的基础文件（幂等）：
//   - rules.txt 默认规则模板（缺失时写入）
//   - GEO 数据库下载（缺失时拉取 geosite.dat / geoip-lite.dat）
//
// 供 GUI 首次启动调用：用户尚未注册时也能看到默认规则与 GEO 状态，
// 而不是必须等 Start（需注册）成功后才生成。CLI 不调用（保持显式行为）。
func (s *Server) InitDefaults(ctx context.Context) error {
	cfg, err := s.ensureConfig()
	if err != nil {
		return err
	}
	// 规则文件：缺失时优先从 GitHub 仓库拉取默认规则（含 REJECT 广告拦截），
	// 下载失败回退内置模板。首次启动下载一次，之后文件已存在不再触碰。
	// （EnsureRulesFile 在 NewEngine 内也会做，但那只在 Start 后；这里独立
	// 触发，用户未注册时也能看到规则页。）
	if err := os.MkdirAll(filepath.Dir(cfg.RulesPath), 0o755); err != nil {
		return fmt.Errorf("创建规则目录失败：%w", err)
	}
	if _, err := os.Stat(cfg.RulesPath); errors.Is(err, fs.ErrNotExist) {
		log.Println("首次启动：正在下载默认规则模板...")
		updated, derr := route.DownloadDefaultRules(ctx, cfg.RulesPath, cfg.AccelerateURL(route.DefaultRulesURL))
		if derr != nil {
			log.Printf("⚠ 默认规则下载失败（回退内置模板）：%v", derr)
			if _, err := route.EnsureRulesFile(cfg.RulesPath); err != nil {
				return fmt.Errorf("初始化默认规则失败：%w", err)
			}
		} else if updated {
			log.Printf("✓ 已下载默认规则 %s", cfg.RulesPath)
		}
	}

	// GEO 下载：缺失时拉取；SHA-1 去重由 route.UpdateGeoData 负责。
	if !geoDataPresent(cfg.GeoDir) {
		if err := os.MkdirAll(cfg.GeoDir, 0o755); err != nil {
			return fmt.Errorf("创建 GEO 目录失败：%w", err)
		}
		updated, uerr := route.UpdateGeoData(ctx, cfg.GeoDir, cfg.GeoSiteURL(), cfg.GeoIPURL())
		if uerr != nil {
			log.Printf("⚠ 初始 GEO 下载失败（可稍后手动更新）：%v", uerr)
			return nil // 下载失败不致命，下次可重试
		}
		if updated {
			log.Println("✓ 初始 GEO 数据已下载")
		}
	}
	return nil
}

// ScanEdges 对 WARP 边缘全段做延迟扫描，返回 RTT 最优的 top-N 端点。
// 供 GUI "扫描最优边缘"按钮调用；需要注册信息（未注册报清晰错误）。
// 扫描只探测不修改注册信息（与 CLI -scan 一致，结果不写回 reg.json）。
func (s *Server) ScanEdges(ctx context.Context) ([]string, error) {
	return s.ScanEdgesFamily(ctx, "")
}

// ScanEdgesFamily 按指定地址族扫描："4"/"6"；空串用 Options.EdgeIP。
func (s *Server) ScanEdgesFamily(ctx context.Context, ipMode string) ([]string, error) {
	regData, err := registration.Load(s.opts.StateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s 中没有注册信息：%w", s.opts.StateFile, ErrNoRegistration)
		}
		return nil, fmt.Errorf("无法读取注册文件 %s：%w", s.opts.StateFile, err)
	}

	verifyEdge, err := regData.PeerPublicKeyVerifier()
	if err != nil {
		return nil, fmt.Errorf("边缘公钥固定初始化失败：%w", err)
	}
	tlsConfig := &tls.Config{
		ServerName:            "consumer-masque-proxy.cloudflareclient.com",
		NextProtos:            []string{"h3"},
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{regData.ClientCert},
		VerifyPeerCertificate: verifyEdge,
		CurvePreferences:      []tls.CurveID{tls.CurveP256, tls.CurveP384, tls.CurveP521},
	}

	// 默认扫注册信息给出的地址族；ipMode 参数（"4"/"6"）优先，
	// 显式 EdgeIP 为 host:port 时用它的地址族段。
	v6 := false
	fallback := []string{}
	edgeSpec := ipMode
	if edgeSpec == "" {
		edgeSpec = s.opts.EdgeIP
	}
	if edgeSpec == "" {
		edgeSpec = "4"
	}
	switch edgeSpec {
	case "4":
		fallback = append(fallback, net.JoinHostPort(regData.EndpointV4, "443"))
	case "6":
		v6 = true
		fallback = append(fallback, net.JoinHostPort(regData.EndpointV6, "443"))
	default:
		// 显式端点：不扫描（与 CLI 一致），返回它本身。
		return []string{edgeSpec}, nil
	}

	// 复用 runEndpointScan：默认段 + 注册端口，RTT 升序 top-N。
	return runEndpointScan(v6, fallback, regData, tlsConfig,
		s.opts.ScanCIDR, s.opts.ScanPorts, s.opts.ScanConcurrency,
		s.opts.ScanTimeout, s.opts.ScanPerProbe, s.opts.ScanTop), nil
}

// geoUpdateOnce 是自动更新协程的调用入口：失败只打 warning，不中断周期。
func (s *Server) geoUpdateOnce(ctx context.Context) {
	updated, err := s.UpdateGeo(ctx)
	if err != nil {
		log.Printf("⚠ GEO 数据更新失败（保留现有数据，下个周期重试）：%v", err)
		return
	}
	if !updated {
		log.Println("✓ GEO 数据已是最新（SHA-1 一致，跳过）")
		return
	}
	log.Println("✓ GEO 数据已更新并热加载")
}

// SetSystemProxy 开启/关闭系统代理，指向当前监听地址。运行中直接调用
// setSystemProxy；停止时记录配置意向并落盘（Start 时按配置应用）。GUI 开关专用。
func (s *Server) SetSystemProxy(enabled bool) error {
	s.mu.Lock()
	listenAddr := s.listenAddr
	running := s.st == stateRunning
	s.mu.Unlock()

	if !running {
		// 未运行：记录配置意向并持久化到 config.json（重启后仍生效）。
		cfg, err := s.ensureConfig()
		if err != nil {
			return err
		}
		s.mu.Lock()
		cfg.EnableSystemProxy = enabled
		s.cfg = cfg
		s.mu.Unlock()
		if err := s.SaveConfig(cfg); err != nil {
			return fmt.Errorf("持久化系统代理设置失败：%w", err)
		}
		log.Printf("✓ 系统代理设置已记录（%v），启动时自动应用", enabled)
		return nil
	}
	if listenAddr == "" {
		return fmt.Errorf("监听地址未知，无法设置系统代理")
	}
	if err := setSystemProxy(listenAddr, enabled); err != nil {
		return err
	}
	s.sysProxyEnabled.Store(enabled)
	if enabled {
		log.Printf("✓ 系统代理已指向 %s", listenAddr)
	} else {
		log.Println("✓ 系统代理已清除")
	}
	return nil
}

// SetEdgeAddr 应用扫描选出的最优边缘地址（写入 config.json edge_addr，
// 下次启动生效；GUI 扫描页"应用"按钮）。
func (s *Server) SetEdgeAddr(addr string) error {
	cfg, err := s.ensureConfig()
	if err != nil {
		return err
	}
	cfg.EdgeAddr = addr
	return s.SaveConfig(cfg)
}

// SetAutostart 开启/关闭开机自启（指向当前可执行文件）。
// 三平台：Windows 注册表 Run / macOS LaunchAgent / Linux autostart .desktop。
func (s *Server) SetAutostart(enabled bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败：%w", err)
	}
	if enabled {
		return autostart.Enable(exe)
	}
	return autostart.Disable()
}

// AutostartEnabled 报告当前是否已注册开机自启。
func (s *Server) AutostartEnabled() bool {
	return autostart.Enabled()
}

// ReloadRules 从磁盘重新加载路由规则（GUI "重新加载"按钮 / 外部编辑后
// 手动触发；与文件热重载同一 applyRules 路径）。
func (s *Server) ReloadRules() error {
	s.mu.Lock()
	k := s.kernel
	s.mu.Unlock()
	if k == nil {
		return fmt.Errorf("分流引擎未初始化")
	}
	return k.engine.get().Reload()
}

// SaveConfig 校验并原子写回 config.json；运行中由 WatchConfig 热重载应用。
// GUI 设置页保存入口。
func (s *Server) SaveConfig(cfg *Config) error {
	path := s.opts.ConfigPath
	if path == "" {
		path = "config.json"
	}
	// 以默认值为基底补齐缺省字段，避免部分字段为零值导致意外关闭特性。
	merged := DefaultConfig()
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("序列化配置失败：%w", err)
		}
		if err := json.Unmarshal(b, merged); err != nil {
			return fmt.Errorf("解析配置失败：%w", err)
		}
	}
	if err := WriteConfig(path, merged); err != nil {
		return err
	}
	log.Printf("✓ 配置已保存 %s（热重载将自动应用）", path)
	return nil
}

// applyConfigReload 把热重载后的配置应用到运行中的进程：
//   - rules_path / geo_dir 变更 → 重建分流引擎（热加载新规则与新 GEO 库）
//   - enable_system_proxy 变更 → 立即设置/清除系统代理
//   - 其余字段（监听地址、UDP 开关、GEO 仓库与更新周期）绑定启动时的监听器
//     与更新协程，变更需重启生效，此处只打日志提示。
func (s *Server) applyConfigReload(old, nc *Config) {
	if nc.RulesPath != old.RulesPath || nc.GeoDir != old.GeoDir {
		ne, err := route.NewEngine(nc.RulesPath, nc.GeoDir)
		if err != nil {
			log.Printf("⚠ 规则/GEO 路径变更后重建引擎失败（保持旧引擎）：%v", err)
		} else {
			s.mu.Lock()
			k := s.kernel
			s.mu.Unlock()
			if k != nil {
				k.engine.swap(ne)
			} else {
				ne.Close()
			}
			log.Printf("✓ 分流引擎已按新路径重建（rules=%s，geo=%s）", nc.RulesPath, nc.GeoDir)
		}
	}

	if nc.EnableSystemProxy != old.EnableSystemProxy {
		if nc.EnableSystemProxy {
			if err := setSystemProxy(s.listenAddr, true); err != nil {
				log.Printf("⚠ 启用系统代理失败：%v", err)
			} else {
				s.sysProxyEnabled.Store(true)
				log.Printf("✓ 系统代理已指向 %s", s.listenAddr)
			}
		} else {
			if err := setSystemProxy(s.listenAddr, false); err != nil {
				log.Printf("⚠ 清除系统代理失败：%v", err)
			} else {
				s.sysProxyEnabled.Store(false)
				log.Println("✓ 系统代理已清除")
			}
		}
	}

	var restart []string
	if nc.ListenAddr != old.ListenAddr {
		restart = append(restart, "listen_addr")
	}
	if nc.AllowUDP != old.AllowUDP {
		restart = append(restart, "allow_udp")
	}
	if nc.GeoRepo != old.GeoRepo {
		restart = append(restart, "geo_repo")
	}
	if nc.GeoAutoUpdateDays != old.GeoAutoUpdateDays {
		restart = append(restart, "geo_auto_update_days")
	}
	if len(restart) > 0 {
		log.Printf("⚠ 配置变更需重启生效：%s", strings.Join(restart, "、"))
	}
}

// geoDataPresent 判断 GEO 数据文件是否已就绪（任一缺失即视为"尚未就绪"）。
func geoDataPresent(geoDir string) bool {
	for _, name := range []string{"geosite.dat", "geoip-lite.dat"} {
		if _, err := os.Stat(filepath.Join(geoDir, name)); err != nil {
			return false
		}
	}
	return true
}

// resolveEdge turns an explicit -ip value into the candidate address list.
//
// A hostname has to be resolved by the system resolver: this runs before the
// tunnel exists, so the in-tunnel DoH client is not available yet. That means a
// hostname here is visible to the local resolver — the same exposure the
// registration API call already has, but worth knowing about. An IP literal
// avoids it entirely.
//
// Every address the name resolves to becomes a candidate, so a dual-stack
// hostname still works on a single-stack host: the families this host cannot
// route are rejected immediately by the dialer.
func resolveEdge(spec string) ([]string, error) {
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, fmt.Errorf("需要 host:port 形式，例如 162.159.198.2:443、"+
			"[2606:4700:103::2]:443 或 example.com:443（%w）", err)
	}
	if host == "" {
		return nil, errors.New("需要 host:port 形式，主机部分为空")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("端口 %q 不是 1-65535 范围内的数字", port)
	}

	if net.ParseIP(host) != nil {
		return []string{net.JoinHostPort(host, port)}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), edgeLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("用系统解析器解析 %q 失败：%w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%q 未解析出任何地址", host)
	}

	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.JoinHostPort(a.IP.String(), port))
	}
	return out, nil
}

// runEndpointScan 在启动前对 WARP 边缘全段做扫描优选，返回替换后的 edgeAddrs。
//
// 行为契约（与方案 §5 对齐）：
//   - v6=true 扫描 IPv6 默认段，否则扫描 IPv4 默认段。额外段经 scanCidr 追加。
//   - 端口集合：scanPorts 非空时覆盖，否则复用 reg.EndpointPorts（与正式
//     连接的端口回退候选集一致，语义自洽）。
//   - 成功：top-N 端点前置 + 注册端点尾接兜底，返回新 edgeAddrs。
//   - 失败：fallback 原样返回，打 warning，不致命 —— 上层照常用注册端点。
//   - 公钥固定：tlsConfig 由 scanner 透传给每个探针，探针内部在 QUIC 握手
//     阶段会调用 VerifyPeerCertificate，故扫描就在 WARP 同组边缘内进行。
//
// 这是启动编排的私有逻辑，不含扫描算法本身 —— 算法在 scanner.Scan。
func runEndpointScan(
	v6 bool,
	fallback []string, // 注册端点（尾接兜底）
	reg *registration.Registration,
	tlsConfig *tls.Config,
	scanCidr, scanPorts string,
	scanConc int,
	scanTimeout, scanPerProbe time.Duration,
	scanTop int,
) []string {
	// 默认段 + 用户追加段。
	cidrs := scanner.DefaultV4CIDRs()
	fam := "IPv4"
	if v6 {
		cidrs = scanner.DefaultV6CIDRs()
		fam = "IPv6"
	}
	if extra, ok := parseCIDRList(scanCidr); ok {
		cidrs = append(cidrs, extra...)
	}

	// 端口：scanPorts 优先，否则复用注册端口。
	ports := reg.EndpointPorts
	if pv, ok := parsePortList(scanPorts); ok {
		ports = pv
	}
	if len(ports) == 0 {
		ports = []int{443}
	}

	// 并发数：钳到"自动 min(64, NumCPU*8)、下限 16"由 scanner.ClampConcurrency
	// 统一负责（scanConc<=0 触发自动）。
	conc := scanner.ClampConcurrency(scanConc)

	log.Printf("扫描 WARP %s 边缘（段=%d 个，端口=%v，并发=%d，总超时=%s，单探针=%s，top=%d）...",
		fam, len(cidrs), ports, conc, scanTimeout, scanPerProbe, scanTop)

	results, err := scanner.Scan(context.Background(), scanner.Config{
		CIDRs:           cidrs,
		Ports:           ports,
		TLSConfig:       tlsConfig,
		QUICConfig:      scanner.ProbeQuicConfig(),
		Concurrency:     conc,
		PerProbeTimeout: scanPerProbe,
		TotalTimeout:    scanTimeout,
		TopN:            scanTop,
		PerIPLimit:      scanner.DefaultPerIPLimit,
	})
	if err != nil {
		log.Printf("⚠ 扫描未得到可用端点（%v），回退到注册端点 %v", err, fallback)
		return fallback
	}

	// top-N 前置，注册端点尾接兜底。
	out := make([]string, 0, len(results)+len(fallback))
	topAddrs := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Addr)
		topAddrs = append(topAddrs, r.Addr)
	}
	out = append(out, fallback...)
	log.Printf("✓ 扫描完成，选用 %d 个最低延迟 %s 端点：%s", len(results), fam, topAddrs)
	if len(fallback) > 0 {
		log.Printf("  注册端点尾接兜底：%v", fallback)
	}
	return out
}

// parseCIDRList 把逗号分隔的 CIDR 字符串解析成切片。空串返回 (nil,false)
// 表示"用户没有指定"，与"用户指定了空列表"区分。非法条目静默丢弃：一个
// 坏段不应让整个扫描停摆（与 BuildCandidates 对非法 CIDR 的处理一致）。
func parseCIDRList(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(p); err != nil {
			log.Printf("⚠ 忽略非法 CIDR 段 %q（%v）", p, err)
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// parsePortList 把逗号分隔的端口字符串解析成切片。空串返回 (nil,false)。
// 非法端口静默丢弃，全部非法则返回 (nil,false) 让上层回退到注册端口。
func parsePortList(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			log.Printf("⚠ 忽略非法端口 %q", p)
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
