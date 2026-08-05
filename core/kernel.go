package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"warp/registration"
	"warp/route"
	"warp/tunnel"
)

// dialer 是 Kernel 的隧道拨号缝：生产实现是 *tunnel.MasqueClient，测试注入
// 假拨号器，避免单测发起真实网络连接。定义在 kernel.go，使 Kernel 只依赖
// tunnel / route / registration，不依赖 proxy 包（反向：Server 同时导入
// proxy 与 Kernel）。
type dialer interface {
	DialTunnel(ctx context.Context, targetAddr string) (net.Conn, error)
	Close() error
}

// engineHolder 持有当前分流引擎，支持整体替换（GEO 更新 / 路径变更后热加载
// 新库与新规则），并保证并发 Match 永远读到一致实例；替换或关闭时旧引擎的
// 规则文件监听随之停掉。定义在 kernel.go（Kernel 自包含），core.go 同包直接使用。
type engineHolder struct {
	mu sync.RWMutex
	e  *route.Engine
}

func (h *engineHolder) get() *route.Engine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.e
}

func (h *engineHolder) swap(e *route.Engine) {
	h.mu.Lock()
	old := h.e
	h.e = e
	h.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// close 停止引擎并可重复调用（与 swap 并发时不会双重 Close 同一实例）。
func (h *engineHolder) close() {
	h.mu.Lock()
	e := h.e
	h.e = nil
	h.mu.Unlock()
	if e != nil {
		e.Close()
	}
}

// Kernel 是可复用的 WARP 内核：MASQUE 隧道客户端 + 分流引擎 + 注册信息。
// 与 Server 的差异：Kernel 不含 mixed 代理监听、系统代理、配置热重载与
// 状态快照 —— 这些留在 Server（CLI/GUI 共用）。Kernel 供 Server 与未来的
// Android 桥（androidvpn）共用：创建即建好隧道与引擎，Start/Stop 管理
// 生命周期，DialTunnel / Route 直接可用。
//
// 线程安全：engine / dial 在构造后不变（engineHolder 与 MasqueClient 各自
// 内部有并发保护）；生命周期状态由 mu 保护；Close 幂等可重复调用。
type Kernel struct {
	cfg       *Config
	regData   *registration.Registration
	edgeAddrs []string
	tlsConfig *tls.Config

	engine *engineHolder // 分流引擎（可整体替换，见 engineHolder）
	dial   dialer        // 隧道拨号器（生产 = *tunnel.MasqueClient）

	mu         sync.Mutex
	started    bool      // Start 已进入生命周期（阻塞等待）
	startTime  time.Time // Start 首次进入的壁钟时间（StartedAt 数据源）
	closed     bool      // Close 已执行（幂等）
	stopClosed bool      // stopCh 已关闭（Stop/Close 触发）
	stopCh     chan struct{}
}

// NewKernel 创建并装配 Kernel：校验入参 → 建立 MASQUE 隧道（NewMasqueClient
// 内部重试到连通，与 Server.Start 原行为一致）→ 创建分流引擎（自动初始化
// 默认 rules.txt 模板、加载规则与 GEO 库——GEO 缺失时降级 rules-only、启动
// 规则文件热重载）。任一步失败都会清理已建资源后返回错误。
//
// 隧道拨号在装配时阻塞重试到连通，不可从外部取消（进程持有隧道生命周期）。
func NewKernel(cfg *Config, regData *registration.Registration, edgeAddrs []string, tlsConfig *tls.Config) (*Kernel, error) {
	return NewKernelContext(context.Background(), cfg, regData, edgeAddrs, tlsConfig)
}

// NewKernelContext 是 NewKernel 的可取消版本：ctx 取消初始 MASQUE 拨号重试
// （NewMasqueClientContext）或在装配开始前已取消时直接失败。Android 桥用它
// 让"装配中停止"能立即中止拨号，而非无限重连停不掉（v0.5.10 反馈）。
// 桌面/CLI 路径用 NewKernel（background，行为不变）。
func NewKernelContext(ctx context.Context, cfg *Config, regData *registration.Registration, edgeAddrs []string, tlsConfig *tls.Config) (*Kernel, error) {
	return newKernel(ctx, cfg, regData, edgeAddrs, tlsConfig, func() (dialer, error) {
		return tunnel.NewMasqueClientContext(ctx, edgeAddrs, tlsConfig, regData.Token)
	})
}

// newKernel 是 NewKernel/NewKernelContext 的拨号缝版本：ctx 传递取消语义
// （NewKernel 传 background），newDial 注入拨号器工厂。生产路径由
// NewKernel/NewKernelContext 提供真实工厂（tunnel.NewMasqueClient[Context]）；
// 测试注入假拨号器避免真实网络连接。
func newKernel(ctx context.Context, cfg *Config, regData *registration.Registration, edgeAddrs []string, tlsConfig *tls.Config, newDial func() (dialer, error)) (*Kernel, error) {
	if cfg == nil {
		return nil, errors.New("kernel: 配置为空")
	}
	if regData == nil {
		return nil, errors.New("kernel: 注册信息为空")
	}
	if len(edgeAddrs) == 0 {
		return nil, errors.New("kernel: 未提供任何边缘地址")
	}
	if tlsConfig == nil {
		return nil, errors.New("kernel: TLS 配置为空")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}

	// 先建隧道（拨号在构造时完成，与 Server.Start 原时序一致），后建引擎；
	// 引擎失败时关闭已建隧道，避免泄漏已建立的连接。
	d, err := newDial()
	if err != nil {
		return nil, err
	}
	eng := &engineHolder{}
	e, err := route.NewEngine(cfg.RulesPath, cfg.GeoDir)
	if err != nil {
		d.Close()
		return nil, err
	}
	eng.e = e
	return &Kernel{
		cfg:       cfg,
		regData:   regData,
		edgeAddrs: edgeAddrs,
		tlsConfig: tlsConfig,
		engine:    eng,
		dial:      d,
		stopCh:    make(chan struct{}),
	}, nil
}

// Start 运行 Kernel 生命周期并阻塞，直到 ctx 被取消或 Stop()/Close() 被
// 调用。幂等：重复调用立即返回 nil。隧道与引擎由 NewKernel 建立（隧道重连、
// 规则热重载都在各自内部完成），Start 只负责监督等待 —— 与 Server.Start 的
// select 语义一致。Android 桥用 go kernel.Start(ctx) + kernel.Stop() 驱动。
func (k *Kernel) Start(ctx context.Context) error {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return errors.New("kernel: 已关闭")
	}
	if k.started {
		k.mu.Unlock()
		return nil
	}
	k.started = true
	k.startTime = time.Now()
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.started = false
		k.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil
	case <-k.stopCh:
		return nil
	}
}

// Stop 请求停止 Kernel 生命周期（幂等，立即返回）。若 Start 尚未调用，
// stopCh 先关闭，之后的 Start 立即返回（与 Server.Stop 的语义一致）。
// 注意：Stop 不拆除隧道与引擎 —— 拆除是 Close 的职责。
func (k *Kernel) Stop() {
	k.signalStop()
}

// signalStop 关闭 stopCh（幂等）。
func (k *Kernel) signalStop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopClosed {
		return
	}
	k.stopClosed = true
	close(k.stopCh)
}

// DialTunnel 通过 WARP 隧道建立到 targetAddr 的字节流连接（分流已判定走
// proxy 时由 proxy 包调用）。Kernel 关闭后返回错误。
func (k *Kernel) DialTunnel(ctx context.Context, targetAddr string) (net.Conn, error) {
	k.mu.Lock()
	d := k.dial
	closed := k.closed
	k.mu.Unlock()
	if closed || d == nil {
		return nil, errors.New("kernel: 隧道不可用")
	}
	return d.DialTunnel(ctx, targetAddr)
}

// Route 判定 host/ip 应走的路径，镜像 proxy 包对 Router 的调用语义：
//   - 命中 "proxy" → ("proxy", true)（走隧道）
//   - 命中 "direct" → ("direct", true)（本地直连）
//   - 未命中 → ("", false)（隐式 direct 兜底；旧 Router 返回 ("direct", false)，
//     对 proxy 行为等价 —— proxy 只看 matched，false 即本地直连）
//   - 引擎未就绪 / 已关闭 → ("", false)
func (k *Kernel) Route(host string, ip netip.Addr) (string, bool) {
	e := k.engine.get()
	if e == nil {
		return "", false
	}
	action, _, matched := e.Match(host, ip)
	if !matched {
		return "", false
	}
	return action, true
}

// ReloadRules 从磁盘重新加载路由规则（GUI "重新加载"按钮 / 外部编辑后手动
// 触发；与文件热重载同一 applyRules 路径）。分流引擎未就绪（未装配 / 已
// 关闭）时返回明确错误。
func (k *Kernel) ReloadRules() error {
	e := k.engine.get()
	if e == nil {
		return errors.New("分流引擎未初始化")
	}
	return e.Reload()
}

// AssignedIPv4 返回 WARP 分配的 IPv4 地址；未分配 / 非法时返回零值 netip.Addr。
func (k *Kernel) AssignedIPv4() netip.Addr {
	if k.regData == nil {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(k.regData.AssignedIPv4)
	return a
}

// Stats 返回分流引擎的命中统计（proxy/direct/reject/miss 计数）。
// Android 状态页从此读真实统计——GUI GetStatus 的 Android 分支从
// androidRuntime.kernel（VpnService 驱动的内核）取数，而非从未启动的
// Server.kernel（nil → 全 0，v0.5.22 修复"流量统计无变化"）。
func (k *Kernel) Stats() route.Stats {
	e := k.engine.get()
	if e == nil {
		return route.Stats{}
	}
	return e.Stats()
}

// Rules 返回分流引擎当前规则数（GUI 状态页"规则条数"数据源）。
// 与 Stats 同属 Android 状态页修复：从真实内核读，而非从未启动的
// Server.kernel（无规则、恒 0）。
func (k *Kernel) Rules() int {
	e := k.engine.get()
	if e == nil {
		return 0
	}
	return len(e.Rules())
}

// StartedAt 返回内核启动时间。Android 桥在装配成功（kernel 写入
// androidRuntime）时自行记录真实开始时间（见 androidbridge.go startTime）并
// 用它填充状态页；此处返回零值供 Server 语义区分（Server.Start 才写入）。
// 未 Start 时返回零值 time.Time。
func (k *Kernel) StartedAt() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.started {
		return time.Time{}
	}
	return k.startTime
}

// AssignedIPv6 返回 WARP 分配的 IPv6 地址；未分配 / 非法时返回零值 netip.Addr。
func (k *Kernel) AssignedIPv6() netip.Addr {
	if k.regData == nil {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(k.regData.AssignedIPv6)
	return a
}

// Close 拆除 Kernel 全部资源（幂等，可重复调用）：唤醒 Start 生命周期等待
// → 关分流引擎 → 拆 MASQUE 隧道。Server.shutdown 与 Android 桥退出时调用。
func (k *Kernel) Close() error {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil
	}
	k.closed = true
	eng := k.engine
	d := k.dial
	k.mu.Unlock()

	k.signalStop() // 唤醒正在 Start 阻塞的等待者
	if eng != nil {
		eng.close()
	}
	if d != nil {
		return d.Close()
	}
	return nil
}
