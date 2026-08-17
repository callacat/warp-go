package route

// 规则解析与持久化：把 rules.txt 的纯文本（每行一条 `行为,条件`）解析为
// Rule 结构，负责默认模板的首次初始化、文件读取与基于轮询的热重载。
//
// 文件格式（与 Mihomo 规则风格对齐）：
//
//	# 注释行（忽略）
//	proxy,geosite:google
//	direct,geoip:cn
//	domain:example.com → 走 direct 兜底（行为缺省不合法，必须显式给出）
//
// 可选兜底声明（至多一条）：
//
//	default:direct   全部规则未命中时使用该行为（proxy/direct/reject）
//	                 ——规则文件优先于代码硬编码的兜底；未声明时由调用方
//	                 （桌面隐式 direct / Android 隧道兜底 proxy）决定。
//
// 条件支持四类：
//
//	geosite:<name>  匹配 geosite 分类的域名（后缀匹配，含子域）
//	geoip:<cc>      匹配 geoip 分类的 IP 段（如 cn）；private 为库内真实条目
//	geoip:lan       内置内网判定（IsPrivate/IsLoopback/IsUnspecified/IsMulticast/IsLinkLocalUnicast）
//	domain:<suffix> 按域名后缀匹配（带标签边界，如 example.com 匹配 a.example.com）

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Action 常量：规则行为。
const (
	ActionProxy  = "proxy"  // 走 WARP 隧道
	ActionDirect = "direct" // 本地直连
	ActionReject = "reject" // 拒绝连接（拦截广告等）
)

// Kind 常量：规则条件类型。
const (
	KindGeoSite = "geosite" // 域名分类匹配
	KindGeoIP   = "geoip"   // IP 段匹配
	KindDomain  = "domain"  // 域名后缀匹配
	// KindDefault 是无条件的兜底声明（`default:<action>` 行），没有匹配
	// 条件：Match 全部未命中时返回其 Action。不参与逐条匹配（无 case 跳过）。
	KindDefault = "default"
)

// Rule 是单条分流规则：行为 + 条件。
// Kind 取值 geosite / geoip / domain；Value 是条件值：
//   - geosite: 分类名（如 google、geolocation-!cn），匹配时大小写不敏感
//   - geoip:   分类名（如 cn、private），lan 是内置特例（不经库）
//   - domain:  域名后缀（如 example.com）
type Rule struct {
	Action string // proxy 或 direct
	Kind   string // geosite / geoip / domain
	Value  string // 条件值（不含前缀）
}

// DefaultRules 是首次初始化写入 rules.txt 的默认规则模板。
// 规则按序匹配，先命中者生效；全部未命中时兜底 direct。
// REJECT 命中即拦截连接（SOCKS5 返回 0x02 / HTTP 返回 403）。
// geolocation-!cn 是 geosite.dat 中的字面类别名（`!` 烘焙进数据，非取反语法）。
const DefaultRules = `# 默认路由规则（每行一条，格式: 行为,条件）
# 行为: proxy = 走 WARP 隧道；direct = 本地直连；reject = 拒绝连接（拦截广告）
# 兜底声明（可选）: default:direct 表示"全部规则未命中时"的行为；未声明时
# 由程序决定（桌面直连 / Android 隧道兜底）。国内网络或 GEO 库未就绪时可用
# default: direct 让未命中流量直连（见 CHANGELOG 阶段 11）。
REJECT,geosite:category-ads-all
direct,geosite:private
direct,geoip:private
proxy,geosite:google
proxy,geoip:google
proxy,geosite:geolocation-!cn
proxy,geoip:telegram
direct,geosite:cn
direct,geoip:cn
`

// 可用的行为与条件类型（大小写不敏感地接受，统一归一化为小写）。
var (
	validActions = map[string]bool{ActionProxy: true, ActionDirect: true, ActionReject: true}
	validKinds   = map[string]bool{KindGeoSite: true, KindGeoIP: true, KindDomain: true}
)

// ParseRules 解析规则文本。空行与 `#` 开头的注释行被忽略；每行格式
// `行为,条件`，行为必须是 proxy/direct，条件必须是 geosite:<name> /
// geoip:<cc> / geoip:lan / geoip:private / domain:<suffix> 之一。
// 另有 `default:<action>` 兜底声明行（至多一条，见 KindDefault）。
// 非法行返回错误，错误信息带行号（从 1 起）。
func ParseRules(rulesText string) ([]Rule, error) {
	var rules []Rule
	lines := strings.Split(rulesText, "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// `default:<action>` 兜底声明：无条件的行为行（default 没有匹配条件），
		// 全部规则未命中时生效。规则文件优先于代码硬编码的兜底（Android
		// 未命中→隧道 proxy）；未声明时调用方按各自语义回退。
		if rest, found := strings.CutPrefix(strings.ToLower(line), "default:"); found {
			action := strings.ToLower(strings.TrimSpace(rest))
			if !validActions[action] {
				return nil, fmt.Errorf("第 %d 行 default 行为 %q 非法（仅支持 proxy/direct/reject）", lineNo, rest)
			}
			for _, r := range rules {
				if r.Kind == KindDefault {
					return nil, fmt.Errorf("第 %d 行：default 只能声明一次（已有 %s）", lineNo, r.Action)
				}
			}
			rules = append(rules, Rule{Action: action, Kind: KindDefault, Value: ""})
			continue
		}

		// 行为与条件之间以逗号分隔；只切第一个逗号，条件值内部不再拆。
		actionPart, condPart, ok := strings.Cut(line, ",")
		if !ok {
			return nil, fmt.Errorf("第 %d 行格式错误：缺少逗号分隔的 `行为,条件`（行内容: %q）", lineNo, line)
		}
		action := strings.ToLower(strings.TrimSpace(actionPart))
		if !validActions[action] {
			return nil, fmt.Errorf("第 %d 行行为 %q 非法（仅支持 proxy 或 direct）", lineNo, actionPart)
		}

		cond := strings.TrimSpace(condPart)
		kind, value, ok := strings.Cut(cond, ":")
		if !ok {
			return nil, fmt.Errorf("第 %d 行条件 %q 非法（缺少 `:` 前缀，如 geosite:google）", lineNo, cond)
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		if !validKinds[kind] {
			return nil, fmt.Errorf("第 %d 行条件类型 %q 非法（仅支持 geosite / geoip / domain）", lineNo, kind)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("第 %d 行条件值不能为空（格式: %s:<name>）", lineNo, kind)
		}

		rules = append(rules, Rule{Action: action, Kind: kind, Value: value})
	}
	return rules, nil
}

// EnsureRulesFile 确保规则文件存在：文件缺失时写入默认模板并返回 true；
// 已存在则原样保留（绝不覆盖用户编辑过的规则）并返回 false。
// 写入采用临时文件 + 原子改名，避免半写文件被热重载读到。
func EnsureRulesFile(path string) (created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("检查规则文件 %s 失败：%w", path, err)
	}
	if err := atomicWriteFile(path, []byte(DefaultRules), 0o644); err != nil {
		return false, fmt.Errorf("写入默认规则文件 %s 失败：%w", path, err)
	}
	return true, nil
}

// LoadRulesFile 读取并解析规则文件。文件缺失时返回 fs.ErrNotExist 包装的错误，
// 解析失败时错误信息带行号。
func LoadRulesFile(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取规则文件 %s 失败：%w", path, err)
	}
	rules, err := ParseRules(string(data))
	if err != nil {
		return nil, fmt.Errorf("解析规则文件 %s 失败：%w", path, err)
	}
	return rules, nil
}

// defaultPollInterval 是热重载轮询间隔。
const defaultPollInterval = 2 * time.Second

// WatchRulesFile 启动基于轮询的规则文件热重载：
//   - 每 defaultPollInterval 检查一次文件的 mtime 与内容 SHA-256，
//     任一变化即重新解析并调用 onReload(rules, err)（err 为解析/读取错误，
//     此时 rules 为 nil）
//   - 返回的停止函数用于退出监听 goroutine；可安全重复调用
//   - 文件暂时消失（如编辑器原子替换的间隙）不触发回调
//
// 调用方负责在启动监听前完成首次加载（如 NewEngine），监听只报告"之后的变更"。
func WatchRulesFile(path string, onReload func(rules []Rule, err error)) (stop func(), err error) {
	return watchRulesFile(path, defaultPollInterval, onReload)
}

// watchRulesFile 是 WatchRulesFile 的可注入间隔版本（测试用短间隔加速）。
func watchRulesFile(path string, interval time.Duration, onReload func(rules []Rule, err error)) (func(), error) {
	// 基线快照：以启动时的文件状态为参照，首个 tick 与它比对。
	baseHash, baseMtime, err := fileState(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("规则文件 %s 不存在（%w）", path, err)
		}
		return nil, fmt.Errorf("读取规则文件 %s 状态失败：%w", path, err)
	}

	stopCh := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		hash, mtime := baseHash, baseMtime
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				nh, nm, err := fileState(path)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue // 原子替换间隙等瞬态，忽略
					}
					// 永久性读取失败（权限等）：上报一次，避免静默失聪
					log.Printf("⚠ 规则文件 %s 状态读取失败：%v", path, err)
					continue
				}
				if nh == hash && nm == mtime {
					continue
				}
				hash, mtime = nh, nm
				rules, rerr := LoadRulesFile(path)
				onReload(rules, rerr)
			}
		}
	}()

	return func() {
		once.Do(func() { close(stopCh) })
	}, nil
}

// fileState 返回文件的 mtime 纳秒值与内容 SHA-256 摘要，用于变更检测。
func fileState(path string) (hash string, mtime int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), info.ModTime().UnixNano(), nil
}

// atomicWriteFile 以"临时文件 + 改名"方式写入，保证读者永远看不到半写内容。
// tmp 文件与目标同目录，确保 rename 在同一文件系统内是原子的。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dirOf(path), ".route-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后无害，失败时清理

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// dirOf 返回 path 的父目录；无路径分隔符时返回 "."。
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}
