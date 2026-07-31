package route

// 匹配引擎：把规则列表与 GEO 数据库组合起来，对 (host, ip) 做分流判定。
//
// 匹配顺序与语义（与 Mihomo 规则风格一致）：
//   - 规则按文件顺序逐条匹配，先命中者生效（first-match-wins）
//   - host 是 IP 字面量时只应用 geoip 类规则
//   - host 是域名时 geosite / domain 规则直接匹配（无需 DNS）；
//     geoip 类规则仅在调用方提供已解析的 ip 时参与
//   - 全部未命中 → 兜底 direct（matched=false）

import (
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
)

// Stats 是匹配引擎的命中计数快照（GUI 状态展示用）。
type Stats struct {
	ProxyHits  int64 // 命中 proxy 规则的次数
	DirectHits int64 // 命中 direct 规则的次数
	Misses     int64 // 未命中任何规则的次数（隐式 direct 兜底）
}

// Engine 组合规则 + GEO 数据库，对外提供 Match 分流判定。
// 并发安全：规则与数据库可被热重载/更新替换，Match 通过 RWMutex 取快照。
type Engine struct {
	mu        sync.RWMutex
	rulesPath string // 规则文件路径（热重载源）
	geoDir    string // GEO 数据库目录
	rules     []Rule
	geoSite   *GeoSiteDB
	geoIP     *GeoIPDB

	stopWatch func() // WatchRulesFile 返回的停止函数，Close 时调用

	// reCache 缓存 geosite 正则条目的编译结果（按模式串）。正则条目在
	// 数据集中极少（个位数），惰性编译 + sync.Map 避免每次匹配重复编译。
	reCache sync.Map // map[string]*regexp.Regexp

	statsProxy  atomic.Int64
	statsDirect atomic.Int64
	statsMiss   atomic.Int64
}

// Match 判定 (host, ip) 的转发行为。
//
//	host: 裸目标主机名（不含端口、不含方括号）；IP 字面量亦可
//	ip:   目标已解析的地址；未解析时传 netip.Addr{}（geoip 规则将被跳过）
//
// 返回 (action, rule, matched)：命中时 matched=true 且返回对应规则；
// 未命中返回 (direct, Rule{}, false)。
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
			domains, ok := geoSite.Lookup(r.Value)
			if !ok {
				continue
			}
			if e.matchGeoSite(domains, lowerHost) {
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

	e.statsMiss.Add(1)
	return ActionDirect, Rule{}, false
}

// hit 记录命中计数并返回规则。
func (e *Engine) hit(r Rule) (string, Rule, bool) {
	if r.Action == ActionProxy {
		e.statsProxy.Add(1)
	} else {
		e.statsDirect.Add(1)
	}
	return r.Action, r, true
}

// matchGeoSite 对分类内全部域名规则做匹配。host 需已小写。
func (e *Engine) matchGeoSite(domains []GeoSiteDomain, host string) bool {
	for _, d := range domains {
		switch d.Type {
		case routercommon.Domain_RootDomain: // 根域后缀：域名本身 + 子域
			if domainSuffixMatch(d.Value, host) {
				return true
			}
		case routercommon.Domain_Full: // 精确
			if d.Value == host {
				return true
			}
		case routercommon.Domain_Plain: // 子串
			if strings.Contains(host, d.Value) {
				return true
			}
		case routercommon.Domain_Regex: // 正则（模式在加载期已小写）
			re := e.compiledRegex(d.Value)
			if re != nil && re.MatchString(host) {
				return true
			}
		}
	}
	return false
}

// compiledRegex 返回模式串对应的编译结果；非法正则返回 nil（并缓存 nil，
// 避免每次匹配都重试编译）。
func (e *Engine) compiledRegex(pattern string) *regexp.Regexp {
	if cached, ok := e.reCache.Load(pattern); ok {
		return cached.(*regexp.Regexp)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		e.reCache.Store(pattern, (*regexp.Regexp)(nil))
		return nil
	}
	e.reCache.Store(pattern, re)
	return re
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
