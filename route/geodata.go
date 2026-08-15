package route

// GEO 数据库解析：把 v2ray protobuf 格式的 geosite.dat（GeoSiteList）与
// geoip-lite.dat（GeoIPList）解码为内存结构，供匹配引擎查询。
//
// 格式定论（勿再调研，见 AGENTS.md §4）：
//   - 两种 .dat 都是 v2ray protobuf，不是 mmdb；解析库为
//     github.com/v2fly/v2ray-core/v5/app/router/routercommon + protobuf.Unmarshal
//   - 类别名在库内大写存储；查询时大小写不敏感（等价于 strings.EqualFold）
//   - 域名类型枚举：Plain=0（子串）/ Regex=1 / RootDomain=2（根域后缀，
//     匹配域名与子域）/ Full=3（精确）
//   - geoip:private 是 geoip-lite.dat 中的真实条目（meta-rules-dat 里约 22 个
//     硬编码 CIDR），不需要代码内置；geoip:lan 才是代码内置检查

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
	"google.golang.org/protobuf/proto"
)

// GeoSiteDomain 是 geosite 分类中的一条域名规则。
type GeoSiteDomain struct {
	Type  routercommon.Domain_Type // 匹配类型（Plain/Regex/RootDomain/Full）
	Value string                   // 域名模式
}

// GeoSiteDB 是内存中的 geosite 数据库：分类名（大写）→ 域名索引。
type GeoSiteDB struct {
	mu         sync.RWMutex
	categories map[string]*geoIndex
	path       string // 来源文件（用于状态展示）
	loadedAt   time.Time
}

// LoadGeoSite 从 geosite.dat 文件解码数据库。proto 校验失败返回错误。
func LoadGeoSite(path string) (*GeoSiteDB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 geosite 数据 %s 失败：%w", path, err)
	}
	return geoSiteFromBytes(data, path)
}

// geoSiteFromBytes 从原始字节解码 geosite 数据库。src 仅用于错误信息与
// 来源标记（download.go 的校验路径传 "<memory>"）。proto.Unmarshal 能
// 识别合法 GeoSiteList 编码，是下载校验的同一道闸。
func geoSiteFromBytes(data []byte, src string) (*GeoSiteDB, error) {
	var list routercommon.GeoSiteList
	if err := proto.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("geosite 数据 %s 不是有效的 v2ray protobuf（GeoSiteList）：%w", src, err)
	}

	db := &GeoSiteDB{
		categories: make(map[string]*geoIndex, len(list.Entry)),
		path:       src,
		loadedAt:   time.Now(),
	}
	for _, entry := range list.Entry {
		if entry == nil || entry.CountryCode == "" {
			continue
		}
		key := strings.ToUpper(entry.CountryCode)
		domains := make([]GeoSiteDomain, 0, len(entry.Domain))
		for _, d := range entry.Domain {
			if d == nil || d.Value == "" {
				continue
			}
			// 域名规则统一小写存储：DNS 大小写不敏感，加载期归一化后
			// 匹配路径零开销（正则条目在数据源里本身就是小写模式）。
			domains = append(domains, GeoSiteDomain{Type: d.Type, Value: strings.ToLower(d.Value)})
		}
		if len(domains) > 0 {
			db.categories[key] = buildGeoIndex(domains)
		}
	}
	return db, nil
}

// Lookup 按分类名查询域名索引。类别名大小写不敏感：查询侧归一化为大写后
// 走 map（与库内大写存储一致，等价于 strings.EqualFold 语义，且是 O(1)）。
func (db *GeoSiteDB) Lookup(name string) (*geoIndex, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	idx, ok := db.categories[strings.ToUpper(name)]
	return idx, ok
}

// CategoryCount 返回已加载的分类数。
func (db *GeoSiteDB) CategoryCount() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.categories)
}

// Path 返回来源文件路径。
func (db *GeoSiteDB) Path() string { return db.path }

// LoadedAt 返回加载时间。
func (db *GeoSiteDB) LoadedAt() time.Time { return db.loadedAt }

// GeoIPDB 是内存中的 geoip 数据库：分类名（大写）→ IP 前缀列表。
// 每个分类的前缀按起始地址排序，匹配时用二分定位 + 逆序扫描：
// 含 ip 的前缀其起始地址必然 ≤ ip，因此从"最后一个起始地址 ≤ ip 的位置"
// 往前扫描即可，通常只需检查极少数前缀。
type GeoIPDB struct {
	mu         sync.RWMutex
	categories map[string][]netip.Prefix
	path       string // 来源文件
	loadedAt   time.Time
}

// LoadGeoIP 从 geoip-lite.dat 文件解码数据库。proto 校验失败返回错误。
func LoadGeoIP(path string) (*GeoIPDB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 geoip 数据 %s 失败：%w", path, err)
	}
	return geoIPFromBytes(data, path)
}

// geoIPFromBytes 从原始字节解码 geoip 数据库（download.go 校验路径共用）。
func geoIPFromBytes(data []byte, src string) (*GeoIPDB, error) {
	var list routercommon.GeoIPList
	if err := proto.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("geoip 数据 %s 不是有效的 v2ray protobuf（GeoIPList）：%w", src, err)
	}

	db := &GeoIPDB{
		categories: make(map[string][]netip.Prefix, len(list.Entry)),
		path:       src,
		loadedAt:   time.Now(),
	}
	for _, entry := range list.Entry {
		if entry == nil || entry.CountryCode == "" {
			continue
		}
		key := strings.ToUpper(entry.CountryCode)
		prefixes := make([]netip.Prefix, 0, len(entry.Cidr))
		for _, cidr := range entry.Cidr {
			if cidr == nil || len(cidr.Ip) == 0 {
				continue
			}
			addr, ok := netip.AddrFromSlice(cidr.Ip)
			if !ok {
				continue // 非法 IP 字节串，跳过
			}
			p := netip.PrefixFrom(addr, int(cidr.Prefix))
			if !p.IsValid() {
				continue
			}
			prefixes = append(prefixes, p.Masked())
		}
		if len(prefixes) == 0 {
			continue
		}
		sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].Addr().Less(prefixes[j].Addr()) })
		db.categories[key] = prefixes
	}
	return db, nil
}

// Lookup 按分类名查询 IP 前缀列表，类别名大小写不敏感。
func (db *GeoIPDB) Lookup(name string) ([]netip.Prefix, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	prefixes, ok := db.categories[strings.ToUpper(name)]
	return prefixes, ok
}

// Contains 判断 ip 是否落在指定分类的任一前缀内。
// 前缀已按起始地址排序：二分找到最后一个起始地址 ≤ ip 的位置后逆序扫描
// （含 ip 的前缀起始地址必然 ≤ ip；私有段等小分类更是几步内命中）。
func (db *GeoIPDB) Contains(name string, ip netip.Addr) bool {
	prefixes, ok := db.Lookup(name)
	if !ok {
		return false
	}
	// 右边界：第一个起始地址 > ip 的下标，即最后一个 ≤ ip 的位置为 i-1。
	i := sort.Search(len(prefixes), func(i int) bool {
		return prefixes[i].Addr().Compare(ip) > 0
	})
	for j := i - 1; j >= 0; j-- {
		if prefixes[j].Contains(ip) {
			return true
		}
	}
	return false
}

// CategoryCount 返回已加载的分类数。
func (db *GeoIPDB) CategoryCount() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.categories)
}

// PrefixCount 返回全部前缀总数（状态展示用）。
func (db *GeoIPDB) PrefixCount() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	n := 0
	for _, ps := range db.categories {
		n += len(ps)
	}
	return n
}

// Path 返回来源文件路径。
func (db *GeoIPDB) Path() string { return db.path }

// LoadedAt 返回加载时间。
func (db *GeoIPDB) LoadedAt() time.Time { return db.loadedAt }
