package route

// geosite 域名索引：把分类内的域名规则从线性扫描 O(N) 加速到 O(标签数)。
//
// 加载期一次性构建，随 GeoSiteDB 整体被热替换（不可变快照）。
// RootDomain 走后缀 map（O(标签数)），Full 走精确 map（O(1)），
// Plain/Regex 占比极低（个位数），保留下沉为线性扫描。

import (
	"regexp"
	"strings"

	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
)

// geoIndex 是单个 geosite 分类的域名索引（构建后只读）。
type geoIndex struct {
	// suffixes 存 RootDomain 条目：Value 做 key，查找时枚举 host 的所有后缀逐个查。
	suffixes map[string]struct{}

	// exact 存 Full 条目：精确匹配 O(1)。
	exact map[string]struct{}

	// plains 存 Plain 条目：子串匹配，线性扫描（占比极低）。
	plains []string

	// regexes 存 Regex 条目：加载期预编译，线性扫描（占比极低）。
	regexes []*regexp.Regexp
}

// buildGeoIndex 把分类内的域名规则构建为索引。
// 域名值需已小写（调用方在 geoSiteFromBytes 中已归一化）。
func buildGeoIndex(domains []GeoSiteDomain) *geoIndex {
	idx := &geoIndex{
		suffixes: make(map[string]struct{}),
		exact:    make(map[string]struct{}),
	}
	for _, d := range domains {
		switch d.Type {
		case routercommon.Domain_RootDomain:
			idx.suffixes[d.Value] = struct{}{}
		case routercommon.Domain_Full:
			idx.exact[d.Value] = struct{}{}
		case routercommon.Domain_Plain:
			idx.plains = append(idx.plains, d.Value)
		case routercommon.Domain_Regex:
			if re, err := regexp.Compile(d.Value); err == nil {
				idx.regexes = append(idx.regexes, re)
			}
		}
	}
	return idx
}

// match 判断 host 是否命中索引中的任一规则。host 需已小写。
//
// 查找顺序：Full 精确 → RootDomain 后缀 → Plain 子串 → Regex 正则。
// RootDomain 枚举 host 的所有后缀（含 host 本身）逐个查 map，O(标签数)。
func (idx *geoIndex) match(host string) bool {
	// 1. Full 精确匹配：O(1)
	if _, ok := idx.exact[host]; ok {
		return true
	}

	// 2. RootDomain 后缀匹配：枚举 host 的所有后缀（含 host 本身）。
	// host = "www.google.com" → 查 "www.google.com"、"google.com"、"com"。
	// 与 domainSuffixMatch 语义一致：标签边界（"." 分隔）。
	if _, ok := idx.suffixes[host]; ok {
		return true
	}
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			if _, ok := idx.suffixes[host[i+1:]]; ok {
				return true
			}
		}
	}

	// 3. Plain 子串：线性扫描（占比极低）。
	for _, p := range idx.plains {
		if strings.Contains(host, p) {
			return true
		}
	}

	// 4. Regex 正则：线性扫描（占比极低，已预编译）。
	for _, re := range idx.regexes {
		if re.MatchString(host) {
			return true
		}
	}

	return false
}

// domainCount 返回索引中的域名规则总数（测试/状态展示用）。
func (idx *geoIndex) domainCount() int {
	return len(idx.suffixes) + len(idx.exact) + len(idx.plains) + len(idx.regexes)
}
