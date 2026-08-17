package route

// 引擎装配与生命周期：NewEngine 完成 规则文件初始化 → 规则加载 → GEO 库
// 尽力加载 → 热重载接线，对外提供 Rules / Reload / UpdateGeo / Stats /
// Close 供 CLI 与 GUI（后续里程碑）共用。

import (
	"context"
	"log"
	"path/filepath"
)

// NewEngine 创建分流引擎：
//   - rulesPath 不存在时写入默认规则模板（DefaultRules）
//   - 加载规则；失败返回错误（启动期必须可见）
//   - 从 geoDir 加载 geosite.dat / geoip-lite.dat：文件缺失或损坏时
//     打 warning 并继续 —— 引擎在 rules-only 模式下仍可用
//     （domain: 规则无需 GEO 数据；geosite/geoip 规则静默不命中）
//   - 启动规则文件热重载（mtime + 内容 hash 轮询，变更自动应用）
func NewEngine(rulesPath, geoDir string) (*Engine, error) {
	created, err := EnsureRulesFile(rulesPath)
	if err != nil {
		return nil, err
	}
	if created {
		log.Printf("✓ 已初始化默认规则文件 %s", rulesPath)
	}

	rules, err := LoadRulesFile(rulesPath)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		rulesPath: rulesPath,
		geoDir:    geoDir,
	}
	// applyRules 统一提取 `default:` 兜底声明（初始加载与热重载同一路径）。
	e.applyRules(rules)

	e.mu.Lock()
	e.loadGeoDBs()
	e.mu.Unlock()

	stop, err := WatchRulesFile(rulesPath, func(rules []Rule, err error) {
		if err != nil {
			log.Printf("⚠ 规则热重载失败（保持旧规则）：%v", err)
			return
		}
		e.applyRules(rules)
		log.Printf("✓ 规则已热重载（%d 条）", len(rules))
	})
	if err != nil {
		return nil, err
	}
	e.stopWatch = stop

	return e, nil
}

// Rules 返回当前生效的规则副本（GUI 编辑展示用）。
func (e *Engine) Rules() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Reload 重新读取并应用规则文件（GUI 手动触发或测试直调；
// 与文件热重载走同一 applyRules 路径）。
func (e *Engine) Reload() error {
	rules, err := LoadRulesFile(e.rulesPath)
	if err != nil {
		return err
	}
	e.applyRules(rules)
	log.Printf("✓ 规则已重新加载（%d 条）", len(rules))
	return nil
}

// applyRules 在写锁下替换规则列表并提取 `default:` 兜底声明
// （规则集中的第一条 KindDefault 条目；无声明时 fallback 为空）。
func (e *Engine) applyRules(rules []Rule) {
	fallback := ""
	for _, r := range rules {
		if r.Kind == KindDefault {
			fallback = r.Action
			break
		}
	}
	e.mu.Lock()
	e.rules = rules
	e.fallback = fallback
	e.mu.Unlock()
}

// UpdateGeo 手动触发 GEO 数据库更新（默认 MetaCubeX/meta-rules-dat 源）。
// 更新成功后把新库热加载进内存；返回是否实际有内容变更。
func (e *Engine) UpdateGeo(ctx context.Context) (bool, error) {
	updated, err := UpdateGeoData(ctx, e.geoDir, DefaultGeoSiteURL, DefaultGeoIPURL)
	if err != nil {
		return updated, err
	}
	if updated {
		e.mu.Lock()
		e.loadGeoDBs()
		e.mu.Unlock()
	}
	return updated, nil
}

// ReloadGeo 从 geoDir 重新加载 GEO 数据库到内存（热加载）。
// 保留规则、统计、规则文件监听不变；仅替换 geoSite/geoIP。
// 下载由调用方完成（Server.UpdateGeo 负责 route.UpdateGeoData），
// 此方法只做内存替换。调用方需保证 geoDir 中的文件已是最新。
func (e *Engine) ReloadGeo() {
	e.mu.Lock()
	e.loadGeoDBs()
	e.mu.Unlock()
}

// Stats 返回命中计数快照（GUI 状态展示用）。
func (e *Engine) Stats() Stats {
	return Stats{
		ProxyHits:    e.statsProxy.Load(),
		DirectHits:   e.statsDirect.Load(),
		RejectedHits: e.statsReject.Load(),
		Misses:       e.statsMiss.Load(),
	}
}

// Close 停止规则热重载监听 goroutine。
func (e *Engine) Close() {
	if e.stopWatch != nil {
		e.stopWatch()
	}
}

// loadGeoDBs 从 geoDir 尽力加载两个 GEO 库；失败只打 warning 不报错
// （rules-only 模式可用）。调用方需持有写锁。
func (e *Engine) loadGeoDBs() {
	sitePath := filepath.Join(e.geoDir, "geosite.dat")
	if site, err := LoadGeoSite(sitePath); err == nil {
		e.geoSite = site
		log.Printf("✓ geosite.dat 已加载（%d 类别）", site.CategoryCount())
	} else {
		e.geoSite = nil
		log.Printf("⚠ geosite.dat 未加载（%v）——geosite 规则将不命中", err)
	}

	ipPath := filepath.Join(e.geoDir, "geoip-lite.dat")
	if ipDB, err := LoadGeoIP(ipPath); err == nil {
		e.geoIP = ipDB
		log.Printf("✓ geoip-lite.dat 已加载（%d 类别 / %d 段）", ipDB.CategoryCount(), ipDB.PrefixCount())
	} else {
		e.geoIP = nil
		log.Printf("⚠ geoip-lite.dat 未加载（%v）——geoip 规则将不命中", err)
	}
}
