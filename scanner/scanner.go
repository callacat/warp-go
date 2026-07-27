package scanner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// ProbeResult 是 Scan 对外暴露的单端点扫描结论，与包内 probeResult 一一对应，
// 仅导出字段，供 main.go 读取 Addr/RTT 拼接 edgeAddrs。
type ProbeResult struct {
	Addr string
	RTT  time.Duration
	OK   bool
}

// Config 描述一次扫描编排。它有两类候选来源，Scan 按优先级取其一：
//
//   - Candidates：直接给出的候选地址列表，最高优先。测试用它注入确定性候选，
//     避免重复算一遍笛卡尔积。
//   - CIDRs + Ports：按 BuildCandidates 展开为 host:port 列表。main.go 走这条。
//
// 两者皆空时 Scan 直接报 errNoCandidates —— 一个没有候选的扫描没有"开机自检"
// 之外的合理产物。
type Config struct {
	// Candidates 直接给出的候选 host:port 列表（如 ["1.1.1.1:443", ...]）。
	// 非空时优先于 CIDRs/Ports 使用。
	Candidates []string

	// CIDRs 是展开候选的源段；与 Ports 配合经 BuildCandidates 笛卡尔积。
	CIDRs []string
	// Ports 是展开候选的端口集合；为空时 Scan 兜底到 [443]，与 reg.json 端口缺失
	// 时的兜底语义一致。
	Ports []int

	// TLSConfig 是 WARP MASQUE 客户端证书 + 边缘公钥固定的 TLS 配置。Scan 只持有
	// 并下发给每个探针，探针内部在拨号时 Clone（见 probe.go:defaultProbeDialer）。
	TLSConfig *tls.Config
	// QUICConfig 是探针用的 quic.Config；推荐由 ProbeQuicConfig() 产出。
	QUICConfig *quic.Config

	// Concurrency 是同时进行的探针数。<=0 时由 Scan 钳到自动默认
	// （min(64, NumCPU*8)，且下限 16），避免无界并发或零并发。
	Concurrency int

	// PerProbeTimeout 限定单个探针的最长耗时。probeEdge 用它派生子 ctx。
	// <=0 时钳到 DefaultPerProbeTimeout。
	PerProbeTimeout time.Duration
	// TotalTimeout 限定整个扫描的最长耗时，到期通过 ctx 取消把尚未完成的探针解锁。
	// <=0 时钳到 DefaultTotalTimeout。
	TotalTimeout time.Duration

	// TopN>0 时 Scan 只返回 RTT 最低的 N 个成功端点；<=0 时返回全部成功端点。
	TopN int

	// PerIPLimit 仅在 CIDRs+Ports 路径上生效（见 BuildCandidates）。
	// <=0 时钳到 DefaultPerIPLimit。Candidates 路径上不用。
	PerIPLimit int
}

// Default 系列是 Scan 自己的钳默认 —— 它们独立于 main.go 暴露给用户的 flag 默认
// （后者是"用户不传时"的值，前者是"Config 字段非法时"的二次钳）。两层默认各管一段，
// 让 main.go 的 flag 解析与 Scan 的契约各自可独立测试与改动。
const (
	DefaultConcurrency     = 64
	DefaultPerProbeTimeout = 3 * time.Second
	DefaultTotalTimeout    = 45 * time.Second
)

// errNoSuccess 表示扫描跑完了候选（或被 TotalTimeout 截断），没有任何一个成功。
// main.go 据此回退到 reg.json 注册端点 —— 这度比"返回空切片 + nil 错误"更直白，
// 因为后者会让上层误以为"扫描成功但无结果"。
var errNoSuccess = errors.New("scanner：无候选成功")

// errNoCandidates 表示 Config 既没给 Candidates 也没给能展开出候选的 CIDRs/Ports。
var errNoCandidates = errors.New("scanner：无候选")

// ProbeQuicConfig 返回探针专用的 quic.Config。
//
// 与 MasqueClient 的 quic.Config（masque.go:179）相比如下，取舍都是"探针只做一次
// 握手测 RTT、不建长连接"的必然结果：
//   - HandshakeIdleTimeout 3s（MasqueClient 是 30s）：探针应快速判失败而非长等。
//   - MaxIdleTimeout 15s（MasqueClient 是 60s）：探针握手一成功就立即关闭，根本
//     到不了空闲超时，给个保险值即可。
//   - EnableDatagrams true：MASQUE 要求，握手阶段就要带上，否则边缘可能拒绝。
//   - InitialPacketSize 1350：与官方、与 MasqueClient 一致。
//   - InitialConnectionReceiveWindow/InitialStreamReceiveWindow 均 1MB（MasqueClient
//     是 10MB/1MB）：探针不传数据，给小窗口省内存，握手包远小于此。
//
// 无 MaxIncomingStreams：探针不接收服务端主动开的流，零即默认，对握手无影响。
func ProbeQuicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 15 * time.Second,
		HandshakeIdleTimeout:           3 * time.Second,
		EnableDatagrams:                true,
		InitialPacketSize:              1350,
		InitialConnectionReceiveWindow: 1 << 20, // 1MB
		InitialStreamReceiveWindow:     1 << 20, // 1MB
	}
}

// Scan 是扫描编排入口。流程：
//
//  1. 包装 TotalTimeout 子 ctx，使其覆盖整个扫描（含族级预探）。
//  2. 解析候选：Candidates 优先，否则由 CIDRs+Ports 经 BuildCandidates 展开。
//  3. 族级预探：对每个出现的地址族发一个代表探针；返回 unroutableFamily 的族，
//     其全部候选被剔除（§7.2）。
//  4. 并发探测：按 Concurrency 用信号量受限并发跑 probeEdge，结果写入对位切片。
//  5. 汇总：过滤成功的、按 RTT 升序、TopN 截断。全失败返回 errNoSuccess。
//
// Scan 不触网：所有真实 QUIC 行为在包级 probeDialer 变量里，测试可注入假实现。
func Scan(ctx context.Context, cfg Config) (results []ProbeResult, err error) {
	// 1. TotalTimeout 子 ctx 覆盖整个扫描，确保预探与主扫描共享同一个取消源。
	// 预探耗时不单独扣除：TotalTimeout 是"整个扫描上限"的语义本就如此。
	total := cfg.TotalTimeout
	if total <= 0 {
		total = DefaultTotalTimeout
	}
	scanCtx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	// 2. 候选解析。
	candidates, err := resolveCandidates(cfg)
	if err != nil {
		return nil, err
	}

	// 字段钳默认。
	concurrency := clampConcurrency(cfg.Concurrency)
	perProbe := cfg.PerProbeTimeout
	if perProbe <= 0 {
		perProbe = DefaultPerProbeTimeout
	}
	topN := cfg.TopN

	// 3. 族级预探：剔除本机整族不可达的候选。
	candidates = familyPreProbe(scanCtx, candidates, cfg.TLSConfig, cfg.QUICConfig, perProbe)
	if len(candidates) == 0 {
		// 预探把所有候选都剔了 —— 说明本机对所有族都不可达。
		return nil, fmt.Errorf("%w（族预探后无候选）", errNoSuccess)
	}

	// 4. 并发探测。
	rawResults := runProbes(scanCtx, candidates, cfg.TLSConfig, cfg.QUICConfig, concurrency, perProbe)

	// 5. 汇总。
	ok := filterSuccess(rawResults)
	if len(ok) == 0 {
		return nil, fmt.Errorf("%w（探测 %d 候选后无成功）", errNoSuccess, len(candidates))
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].RTT < ok[j].RTT })
	if topN > 0 && len(ok) > topN {
		ok = ok[:topN]
	}
	return ok, nil
}

// resolveCandidates 把 Config 的两类候选来源收敛成单一 host:port 列表。
// Candidates 优先；为空时由 CIDRs+Ports 经 BuildCandidates 展开。两者皆空
// 返回 errNoCandidates。
func resolveCandidates(cfg Config) ([]string, error) {
	if len(cfg.Candidates) > 0 {
		return cfg.Candidates, nil
	}
	if len(cfg.CIDRs) == 0 {
		return nil, errNoCandidates
	}
	ports := cfg.Ports
	if len(ports) == 0 {
		ports = DefaultPorts(nil)
	}
	perIP := cfg.PerIPLimit
	if perIP <= 0 {
		perIP = DefaultPerIPLimit
	}
	return BuildCandidates(cfg.CIDRs, ports, perIP), nil
}

// ClampConcurrency 把 Concurrency 钳到 min(64, NumCPU*8)，且以 16 为下限。
// <=0 触发自动；正数但超过自动上限时仍按自动上限收，避免弱机退化、强机失控。
// 导出供 main.go 在日志中展示 flag 钳后的真实并发值，避免 main.go 重复实现同一
// 算法（"两层默认各管一段"：main.go 只决定"用户传了什么"，钳规则归 scanner）。
func ClampConcurrency(c int) int {
	auto := runtime.NumCPU() * 8
	if auto > 64 {
		auto = 64
	}
	if auto < 16 {
		auto = 16
	}
	if c <= 0 || c > auto {
		return auto
	}
	return c
}

// clampConcurrency 是 Scan 内部使用的别名，保留包内调用点无需改动。
func clampConcurrency(c int) int { return ClampConcurrency(c) }

// familyPreProbe 对每个出现的地址族发一个代表探针；返回 unroutableFamily 的族
// 被整体跳过。这个步骤的目的是 §7.2 的"快速失败"：IPv4-only 主机选 IPv6 边缘时，
// 逐端口 ENETUNREACH 会刷爆总超时，故先发一次预探，整族不可达则剔除。
//
// 返回：剔除后剩余的候选。若预探自身 ctx 被父取消，保留全部候选交主扫描处理。
// 直接调用包级 probeDialer 而非 probeEdge，是为了保留类型化 error 去判
// unroutableFamily —— probeEdge 会把 error 文本化进 ErrReason，丢失 errors.Is。
func familyPreProbe(ctx context.Context, candidates []string, tlsCfg *tls.Config, quicCfg *quic.Config, perProbe time.Duration) []string {
	if len(candidates) <= 1 {
		// 单候选无族可言，直接保留，跳过预探省一次往返。
		return candidates
	}

	// 解析每个候选，按族收集首个代表。解析失败的候选留主扫描报错。
	type parsed struct {
		original string
		udp      *net.UDPAddr
	}
	var parsedAll []parsed
	byFam := make(map[bool]string) // key: isIPv6 → 代表候选原文
	var famOrder []bool
	for _, c := range candidates {
		ua, err := net.ResolveUDPAddr("udp", c)
		if err != nil {
			continue
		}
		parsedAll = append(parsedAll, parsed{original: c, udp: ua})
		isV6 := ua.IP.To4() == nil
		if _, ok := byFam[isV6]; !ok {
			byFam[isV6] = c
			famOrder = append(famOrder, isV6)
		}
	}
	if len(byFam) == 0 {
		return candidates
	}

	// 预探代表并发跑（候选族最多 2 个）。仍受 ctx（TotalTimeout）约束。
	skipFams := make(map[bool]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, isV6 := range famOrder {
		repc := byFam[isV6]
		ua, err := net.ResolveUDPAddr("udp", repc)
		if err != nil {
			continue
		}
		isV6 := isV6
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 代表探针也走 perProbeTimeout 子 ctx，与主探针一致。
			pctx, cancel := context.WithTimeout(ctx, perProbe)
			defer cancel()
			_, derr := probeDialer(pctx, ua, tlsCfg, quicCfg)
			if derr != nil && unroutableFamily(derr) {
				mu.Lock()
				skipFams[isV6] = true
				mu.Unlock()
				log.Printf("scanner：地址族 %s 不可达（代表 %s：%v），整族跳过",
					famName(isV6), repc, derr)
			}
		}()
	}
	wg.Wait()

	// 父 ctx 已取消则保守不剔除，交主扫描处理（让 TotalTimeout 逻辑接管）。
	if ctx.Err() != nil {
		return candidates
	}

	// 按族是否被 skip 过滤候选。解析失败的候选（不在 parsedAll 里）也保留，
	// 由主扫描逐个判定 —— 族预探只负责"整族网络不可达"，不负责"单候选坏地址"。
	keepOrig := make(map[string]bool, len(parsedAll))
	for _, p := range parsedAll {
		isV6 := p.udp.IP.To4() == nil
		if skipFams[isV6] {
			continue
		}
		keepOrig[p.original] = true
	}
	remaining := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if keepOrig[c] {
			remaining = append(remaining, c)
		} else if _, err := net.ResolveUDPAddr("udp", c); err != nil {
			// 解析失败的候选也保留：族预探不替它下结论。
			remaining = append(remaining, c)
		}
	}
	if len(skipFams) > 0 {
		log.Printf("scanner：族预探剔除 %d 个族，%d → %d 候选",
			len(skipFams), len(candidates), len(remaining))
	}
	return remaining
}

// runProbes 按 Concurrency 用带缓冲 channel 作信号量，对每候选并发跑 probeEdge，
// 把结果写入对位切片 results[i]。TotalTimeout/取消经 ctx 传给每个 probeEdge。
//
// 候选字符串先 ResolveUDPAddr 成 *net.UDPAddr，再传给 probeEdge —— 因为
// defaultProbeDialer 会对 addr 做 addr.(*net.UDPAddr) 类型断言，传原始字符串
// 适配器会断言失败。每个预解析失败的 slot 直接判失败不上 worker。
//
// 每个 worker 强制 r.Addr = candidates[i]，保证结果地址与候选字符串完全一致
// （TestScan_PreservesAddrInResult 依赖这一点）。
func runProbes(ctx context.Context, candidates []string, tlsCfg *tls.Config, quicCfg *quic.Config, concurrency int, perProbe time.Duration) []probeResult {
	results := make([]probeResult, len(candidates))
	for i, c := range candidates {
		results[i] = probeResult{Addr: c} // 预置 Addr，便于未拨号 slot 也有可读地址
	}

	// 预解析：失败的候选直接落一个失败结果，不占 worker slot。
	type job struct {
		i    int
		addr *net.UDPAddr
	}
	jobs := make([]job, 0, len(candidates))
	for i, c := range candidates {
		ua, err := net.ResolveUDPAddr("udp", c)
		if err != nil {
			results[i] = probeResult{Addr: c, ErrReason: fmt.Sprintf("解析候选失败：%v", err)}
			continue
		}
		jobs = append(jobs, job{i: i, addr: ua})
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var sawCtxErr atomic.Bool

	// 派发循环的两条互斥取槽路径必须严格区分（对抗审查交叉确认的 Critical fix）：
	//   - sem<-struct{}{} 成功：本帧确实占了一个槽，后续任何"决定不派发"的路径
	//     都必须 <-sem 把槽还回去，否则槽泄漏。
	//   - <-ctx.Done() 先到：本帧根本没占槽，**绝对不能** <-sem——否则会取走某在途
	//     worker 即将释放的槽，使该 worker 的 defer <-sem 永远拿不到、wg.Wait 挂死。
	// 用 label dispatchLoop 跳出整个 for，杜绝把 break 误用在 select 内（Go 里
	// break 在 select 只退 select 不退 for，是本 bug 的根因）。
dispatchLoop:
	for _, j := range jobs {
		// 父 ctx 已取消则不再派发：在途的会随 ctx 取消自行退出。
		if ctx.Err() != nil {
			sawCtxErr.Store(true)
			break dispatchLoop
		}
		// 取槽；select 配 ctx 让"取信号量"本身也可被取消，避免 ctx 已取消时仍卡在
		// 阻塞的 sem<-struct{}{} 上。
		gotSlot := false
		select {
		case sem <- struct{}{}:
			gotSlot = true
		case <-ctx.Done():
			sawCtxErr.Store(true)
			break dispatchLoop // 没占槽，不还槽，直接停
		}
		// 占到槽后才发现 ctx 已取消：把刚占的槽还回去再停。
		if ctx.Err() != nil {
			if gotSlot {
				<-sem
			}
			sawCtxErr.Store(true)
			break dispatchLoop
		}
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := probeEdge(ctx, j.addr, tlsCfg, quicCfg, perProbe)
			r.Addr = candidates[j.i] // 强制用候选原文，杜绝改写
			results[j.i] = r
		}()
	}
	wg.Wait()

	// 诊断日志：不参与决策，仅供排查"扫描为何没跑满候选"。
	if sawCtxErr.Load() {
		log.Printf("scanner：探针派发期间 ctx 被取消（%v）", ctx.Err())
	}
	return results
}

// filterSuccess 返回 OK 的结果，保持原顺序（随后由调用方排序）。
func filterSuccess(rs []probeResult) []ProbeResult {
	out := make([]ProbeResult, 0, len(rs))
	for _, r := range rs {
		if r.OK {
			out = append(out, ProbeResult{Addr: r.Addr, RTT: r.RTT, OK: true})
		}
	}
	return out
}

// famName 给日志一个人类可读的族名。
func famName(isV6 bool) string {
	if isV6 {
		return "IPv6"
	}
	return "IPv4"
}
