package core

// 检查更新：查 GitHub Releases API 获取最新版本，与当前版本对比。
// 仅做版本探测（返回最新版与下载页 URL），不自动下载/安装。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UpdateInfo 是检查更新的结果。
type UpdateInfo struct {
	Current   string `json:"current"` // 当前版本（不带 v，如 0.5.3）
	Latest    string `json:"latest"`  // 最新版本（不带 v）
	HasUpdate bool   `json:"has_update"`
	URL       string `json:"url"` // 最新 Release 页面（浏览器打开下载）
	Tag       string `json:"tag"` // 最新 tag（带 v，如 v0.5.3）
}

// checkUpdateTimeout 单次检查超时（网络受限环境快速失败，不阻塞 GUI）。
const checkUpdateTimeout = 15 * time.Second

// updateAPIRepo 默认仓库；测试可覆盖（updater_test.go）。
var updateAPIRepo = "callacat/warp-go"

// updateHTTPClient 检查更新专用客户端（超时在每次请求上设置）。
var updateHTTPClient = &http.Client{}

// CheckUpdate 查询 GitHub Releases API 的最新版本，与 current 比较。
// current 形如 "0.5.3"（来自 -X 注入的 main.version）。
// 网络失败/API 不可达时返回错误（GUI 显示"检查失败"，非致命）。
func CheckUpdate(ctx context.Context, current string) (*UpdateInfo, error) {
	repo := updateAPIRepo
	if repo == "" {
		repo = "callacat/warp-go"
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	ctx, cancel := context.WithTimeout(ctx, checkUpdateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "warp-go/updater")

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询最新版本失败：%w", err)
	}
	defer resp.Body.Close()

	// 未认证请求有 60 次/小时限流；GitHub 对未认证 API 也可能 403。
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API 返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析最新版本响应失败：%w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	info := &UpdateInfo{
		Current: current,
		Latest:  latest,
		URL:     rel.HTMLURL,
		Tag:     rel.TagName,
	}
	if current != "" && latest != "" && current != "dev" {
		info.HasUpdate = compareVersions(latest, current) > 0
	}
	return info, nil
}

// compareVersions 比较两个语义版本号（a > b 返回 1，a < b 返回 -1，相等返回 0）。
// 支持 x.y.z（缺省段视为 0）；非数字段按 0 处理（防御 API 异常 tag）。
func compareVersions(a, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// parseVersion 把 "0.5.3" 拆成 [主, 次, 补丁]；非法段按 0，
// 预发布后缀（"0.5.3-rc1" → "3-rc1" → "3"）忽略。
func parseVersion(v string) [3]int {
	var out [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		seg := parts[i]
		if idx := strings.IndexByte(seg, '-'); idx >= 0 {
			seg = seg[:idx]
		}
		n, err := strconv.Atoi(strings.TrimSpace(seg))
		if err != nil {
			n = 0
		}
		out[i] = n
	}
	return out
}
