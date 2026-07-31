package route

// GEO 数据库下载与更新：从远端拉取 geosite.dat / geoip-lite.dat，
// 依次做 SHA-1 去重 → protobuf 结构校验 → 原子改名落盘，避免半写文件
// 被加载线程读到。
//
// 默认仓库为 MetaCubeX/meta-rules-dat（其 latest release 持续构建
// v2ray 格式的 geosite.dat 与 geoip-lite.dat）。

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
	"google.golang.org/protobuf/proto"
)

// 默认 GEO 数据库下载地址。
const (
	DefaultGeoSiteURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat"
	DefaultGeoIPURL   = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip-lite.dat"
)

// DefaultRulesURL 是仓库内置默认规则文件（rules/default-rules.txt）的 raw 地址。
// 首次启动时优先从这里拉取最新规则模板，下载失败回退内置 DefaultRules。
const DefaultRulesURL = "https://raw.githubusercontent.com/callacat/warp-go/main/rules/default-rules.txt"

// 下载超时与单次读取上限（防异常远端无限拖流）。
const (
	geoDownloadTimeout = 5 * time.Minute
	geoMaxBytes        = 256 << 20 // 256 MiB 上限，远超现有数据量（geosite ≈ 4 MB）
)

// updateHTTPClient 是 GEO 下载专用客户端（超时在每次请求上单独设置）。
var updateHTTPClient = &http.Client{}

// UpdateGeoData 更新 GEO 数据库：下载 siteURL 与 ipURL 两个文件到 geoDir，
// 返回 (updated, err)。
//
//   - 每个文件先 SHA-1 比对：与已存在文件摘要一致则跳过（去重，不写盘）
//   - 新内容用 proto.Unmarshal 校验结构（GeoSiteList / GeoIPList），
//     校验失败视为损坏，报错且不落盘
//   - 通过后临时文件 + 原子改名写入 geoDir/geosite.dat、geoDir/geoip-lite.dat
//   - 任一文件失败即中止并返回错误（不留下半套新数据）；
//     已成功替换的文件保持新版本（与"整包失败回滚"相比，半套新数据仍可用，
//     且下一次更新会补齐）
//   - updated=true 表示至少有一个文件实际写入了新内容
func UpdateGeoData(ctx context.Context, geoDir string, siteURL, ipURL string) (bool, error) {
	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		return false, fmt.Errorf("创建 GEO 目录 %s 失败：%w", geoDir, err)
	}

	sitePath := filepath.Join(geoDir, "geosite.dat")
	ipPath := filepath.Join(geoDir, "geoip-lite.dat")

	updated := false

	if siteURL != "" {
		ok, err := updateOne(ctx, geoDir, siteURL, sitePath, validateGeoSite)
		if err != nil {
			return updated, err
		}
		updated = updated || ok
	}

	if ipURL != "" {
		ok, err := updateOne(ctx, geoDir, ipURL, ipPath, validateGeoIP)
		if err != nil {
			return updated, err
		}
		updated = updated || ok
	}

	return updated, nil
}

// updateOne 下载单个 GEO 文件并落盘，返回是否实际更新。
func updateOne(ctx context.Context, geoDir, url, dst string, validate func([]byte) error) (bool, error) {
	data, err := download(ctx, url)
	if err != nil {
		return false, err
	}

	// SHA-1 去重：与现有文件摘要一致则跳过（更新频率高于数据发布频率的常态路径）。
	if existing, err := os.ReadFile(dst); err == nil {
		if sha1.Sum(existing) == sha1.Sum(data) {
			log.Printf("✓ %s 已是最新（SHA-1 一致，跳过）", filepath.Base(dst))
			return false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("读取现有 GEO 文件 %s 失败：%w", dst, err)
	}

	// protobuf 结构校验：损坏的数据拒绝落盘。
	if err := validate(data); err != nil {
		return false, fmt.Errorf("下载的 %s 校验失败：%w", filepath.Base(dst), err)
	}

	if err := atomicWriteFile(dst, data, 0o644); err != nil {
		return false, fmt.Errorf("写入 GEO 文件 %s 失败：%w", dst, err)
	}
	log.Printf("✓ %s 已更新（%d 字节）", filepath.Base(dst), len(data))
	return true, nil
}

// download 拉取 url 的完整内容。context 取消/超时即中止。
func download(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, geoDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造下载请求 %s 失败：%w", url, err)
	}
	// 显式声明 UA：GitHub release 下载对默认 Go UA 偶有限流。
	req.Header.Set("User-Agent", "warp-go/route (geodata updater)")

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败：HTTP %s", url, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, geoMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 响应体失败：%w", url, err)
	}
	return data, nil
}

// validateGeoSite 校验数据能解码为 GeoSiteList。
func validateGeoSite(data []byte) error {
	var list routercommon.GeoSiteList
	if err := proto.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("不是有效的 v2ray protobuf（GeoSiteList）：%w", err)
	}
	return nil
}

// validateGeoIP 校验数据能解码为 GeoIPList。
func validateGeoIP(data []byte) error {
	var list routercommon.GeoIPList
	if err := proto.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("不是有效的 v2ray protobuf（GeoIPList）：%w", err)
	}
	return nil
}

// validateRules 校验下载的规则文本能被 ParseRules 解析（结构校验，等价于
// GEO 的 proto.Unmarshal 门禁）。
func validateRules(data []byte) error {
	if _, err := ParseRules(string(data)); err != nil {
		return fmt.Errorf("不是有效的规则文本：%w", err)
	}
	return nil
}

// DownloadDefaultRules 从 url 下载默认规则文件并落盘到 path。
// 与 GEO 更新同一管线：SHA-1 去重 → ParseRules 结构校验 → 原子改名。
// 返回是否实际写入。下载或校验失败返回错误，不动现有文件。
func DownloadDefaultRules(ctx context.Context, path, url string) (bool, error) {
	if url == "" {
		return false, fmt.Errorf("默认规则下载地址为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("创建规则目录失败：%w", err)
	}
	updated, err := updateOne(ctx, filepath.Dir(path), url, path, validateRules)
	if err != nil {
		return false, err
	}
	return updated, nil
}
