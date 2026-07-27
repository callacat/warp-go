package main

import (
	"strings"
	"testing"
)

// decideRotate 是 main 包把 -rotate 编排成 (size, auto, err) 的唯一决策点。
// 本测试锁住矩阵的关键路径（非穷举笛卡尔积——业务契约分支而非输入组合）：
//   - 「智能默认开」：rotateArg=false 且 socks5+scan+ip∈{4,6}+scanTop>1 → size=scanTop, auto=true
//   - 「零代价回退」：rotateArg=false 且任一条件不足 → size=0, auto=false
//   - 「fail-fast 矩阵」：rotateArg=true 且缺 socks5 / 缺 scan / 显式 ip / scanTop<=1 → 各自精确错误
//   - 「手动开成功」：rotateArg=true 且全条件满足 → size=scanTop, auto=false（日志区分「手动」）
//
// 纯函数无 I/O：测试直接断言出参三元组，不触 log.Fatalf（那由 main 调用方接 err 后做）。

func TestDecideRotate(t *testing.T) {
	cases := []struct {
		name      string
		socks5    string
		scan      bool
		ip        string
		rotateArg bool
		scanTop   int
		wantSize  int
		wantAuto  bool
		wantErr   string // 错误信息子串（空表示期望无错）
	}{
		// —— auto 智能默认分支 ——
		{
			name: "auto 启用：socks5+scan+ip4+scanTop4",
			socks5: "your-socks5-host:7890", scan: true, ip: "4", rotateArg: false, scanTop: 4,
			wantSize: 4, wantAuto: true, wantErr: "",
		},
		{
			name: "auto 启用：ip6 边缘",
			socks5: "h:1", scan: true, ip: "6", rotateArg: false, scanTop: 8,
			wantSize: 8, wantAuto: true, wantErr: "",
		},

		// —— auto 关闭（零代价回退）的各缺口 ——
		{
			name: "auto 关：无 socks5",
			socks5: "", scan: true, ip: "4", rotateArg: false, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "",
		},
		{
			name: "auto 关：无 scan",
			socks5: "h:1", scan: false, ip: "4", rotateArg: false, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "",
		},
		{
			name: "auto 关：显式 ip 端点",
			socks5: "h:1", scan: true, ip: "162.159.198.2:4500", rotateArg: false, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "",
		},
		{
			name: "auto 关：scanTop<=1 池太小",
			socks5: "h:1", scan: true, ip: "4", rotateArg: false, scanTop: 1,
			wantSize: 0, wantAuto: false, wantErr: "",
		},

		// —— rotateArg=true fail-fast 矩阵（每个缺口给精确错误）——
		{
			name: "fail-fast：rotate=true 但无 socks5",
			socks5: "", scan: true, ip: "4", rotateArg: true, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "-rotate 需 -socks5",
		},
		{
			name: "fail-fast：rotate=true 但无 scan",
			socks5: "h:1", scan: false, ip: "4", rotateArg: true, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "-rotate 需 -scan",
		},
		{
			name: "fail-fast：rotate=true 但显式 ip 端点",
			socks5: "h:1", scan: true, ip: "1.2.3.4:443", rotateArg: true, scanTop: 4,
			wantSize: 0, wantAuto: false, wantErr: "-rotate 需 -scan 扫描结果",
		},
		{
			name: "fail-fast：rotate=true 但 scanTop<=1",
			socks5: "h:1", scan: true, ip: "4", rotateArg: true, scanTop: 1,
			wantSize: 0, wantAuto: false, wantErr: "-rotate 需 -scan-top",
		},

		// —— 手动开成功 ——
		{
			name: "手动开：rotate=true 全条件满足",
			socks5: "h:1", scan: true, ip: "6", rotateArg: true, scanTop: 4,
			wantSize: 4, wantAuto: false, wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, auto, err := decideRotate(tc.socks5, tc.scan, tc.ip, tc.rotateArg, tc.scanTop)

			// 错误断言：要么期望错且 err 含 wantErr，要么期望无错且 err==nil。
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望错误含 %q，实际无错", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误期望含 %q，实际 %q", tc.wantErr, err.Error())
				}
				// fail-fast 时 size 应退回 0（不让建池跑半步）。
				if size != 0 {
					t.Fatalf("fail-fast 错误时 size 应 0，实际 %d", size)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望无错，实际 %v", err)
			}
			if size != tc.wantSize {
				t.Fatalf("size=%d，want %d", size, tc.wantSize)
			}
			if auto != tc.wantAuto {
				t.Fatalf("auto=%v，want %v", auto, tc.wantAuto)
			}
		})
	}
}
