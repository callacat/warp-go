package route

// 匹配引擎：把规则列表与 GEO 数据库组合起来，对 (host, ip) 做分流判定。
//
// 匹配顺序与语义（与 Mihomo 规则风格一致）：
//   - 规则按文件顺序逐条匹配，先命中者生效（first-match-wins）
//   - host 是 IP 字面量时只应用 geoip 类规则
//   - host 是域名时 geosite / domain 规则直接匹配（无需 DNS）；
//     geoip 类规则仅在调用方提供已解析的 ip 时参与
//   - 全部未命中 → 规则文件声明 `default:` 则按其行为（matched=true），
//     否则兜底 direct（matched=false，调用方决定）

import (
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
)

// Stats 是匹配引擎的命中计数快照（GUI 状态展示用）。
// json tag 与前端 ProxyCounters 键对齐（proxy/direct/rejected/miss），
// 生成 bindings 时 TS 属性名即为此 tag（见 frontend/bindings/warp/route/models.ts）。
type Stats struct {
	ProxyHits    int64 `json:"proxy"`    // 命中 proxy 规则的次数
	DirectHits   int64 `json:"direct"`   // 命中 direct 规则的次数
	RejectedHits int64 `json:"rejected"` // 命中 reject 规则的次数（被拦截的连接）
	Misses       int64 `json:"miss"`     // 未命中任何规则的次数（隐式 direct 兜底）
}

// Engine 组合规则 + GEO 数据库，对外提供 Match 分流判定。
// 并发安全：规则与数据库可被热重载/更新替换，Match 通过 RWMutex 取快照。
type Engine struct {
	mu        sync.RWMutex
	rulesPath string // 规则文件路径（热重载源）
	geoDir    string // GEO 数据库目录
	rules     []Rule
	// fallback 是 `default:<action>` 行声明的兜底行为（空 = 未声明）。
	// 未命中任何规则时返回它（matched=true）；未声明时返回 ("direct", false)，
	// 由调用方决定（桌面隐式 direct / Android 隧道 proxy）。
	fallback string
	geoSite  *GeoSiteDB
	geoIP    *GeoIPDB

	stopWatch func() // WatchRulesFile 返回的停止函数，Close 时调用

	statsProxy   atomic.Int64
	statsDirect  atomic.Int64
	statsReject  atomic.Int64
	statsMiss    atomic.Int64
}

// Match 判定 (host, ip) 的转发行为。
//
//	host: 裸目标主机名（不含端口、不含方括号）；IP 字面量亦可
//	ip:   目标已解析的地址；未解析时传 netip.Addr{}（geoip 规则将被跳过）
//
// 返回 (action, rule, matched)：命中时 matched=true 且返回对应规则；
// 全部未命中且规则文件声明了 `default:` → 返回其行为（matched=true）；
// 否则返回 (direct, Rule{}, false)，由调用方决定兜底。
func (e *Engine) Match(host string, ip netip.Addr) (string, Rule, bool) {
	host = strings.TrimSuffix(host, ".")

	// host 为 IP 字面量：只走 geoip 规则（geosite/domain 的域名语义对 IP 无意义）。
	if addr, err := netip.ParseAddr(host); err == nil {
		ip = addr
		host = ""
	}
	lowerHost := strings.ToLower(host)

	e.mu.RLock()
	rules := e.rules
	geoSite := e.geoSite
	geoIP := e.geoIP
	fallback := e.fallback
	e.mu.RUnlock()

	for _, r := range rules {
		switch r.Kind {
		case KindGeoSite:
			if host == "" {
				continue
			}
			if geoSite == nil {
				continue
			}
			idx, ok := geoSite.Lookup(r.Value)
			if !ok {
				continue
			}
			if idx.match(lowerHost) {
				return e.hit(r)
			}

		case KindDomain:
			if host == "" {
				continue
			}
			if domainSuffixMatch(strings.ToLower(r.Value), lowerHost) {
				return e.hit(r)
			}

		case KindGeoIP:
			if !ip.IsValid() {
				continue // 域名且未解析：调用方未提供 IP，geoip 规则无法参与
			}
			if strings.EqualFold(r.Value, "lan") {
				if isLANAddr(ip) {
					return e.hit(r)
				}
				continue
			}
			// geoip:private 走这里 —— private 是 geoip-lite.dat 中的真实类别。
			if geoIP != nil && geoIP.Contains(r.Value, ip) {
				return e.hit(r)
			}
		}
	}

	// 未命中：规则文件显式声明了 `default:` 兜底 → 规则文件优先于代码硬编码
	// （Android 未命中→隧道 proxy 的 hardcode 仅作未声明时的回退）。命中计数
	// 仍按 miss 统计（fallback 本质是"未被规则覆盖"的流量）。
	e.statsMiss.Add(1)
	if fallback != "" {
		return fallback, Rule{Action: fallback, Kind: KindDefault}, true
	}
	return ActionDirect, Rule{}, false
}

// hit 记录命中计数并返回规则。
func (e *Engine) hit(r Rule) (string, Rule, bool) {
	switch r.Action {
	case ActionProxy:
		e.statsProxy.Add(1)
	case ActionReject:
		e.statsReject.Add(1)
	default:
		e.statsDirect.Add(1)
	}
	return r.Action, r, true
}

// domainSuffixMatch 判断 host 是否等于 suffix 或是其子域（标签边界）。
// 与 v2ray strmatcher.DomainMatcher 语义一致（对 IP 无意义，调用方保证
// host 为域名）。host 与 suffix 需均已小写。
func domainSuffixMatch(suffix, host string) bool {
	if len(host) < len(suffix) {
		return false
	}
	if host == suffix {
		return true
	}
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	return host[len(host)-len(suffix)-1] == '.'
}

// isLANAddr 是 geoip:lan 的内置判定：私有/未指定/回环/组播/链路本地。
// 与 netip 的 IsPrivate 等判据直接组合，不查 GEO 库（库内没有 lan 类别）。
func isLANAddr(ip netip.Addr) bool {
	return ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsMulticast() ||
		ip.IsLinkLocalUnicast()
}
