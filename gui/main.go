//go:generate goversioninfo -64
package main

// warp-go GUI 入口。Wails v3 桌面壳：
//   - 主窗口（900x600 起，可缩放，响应式前端适配常用分辨率）
//   - 系统托盘（状态 / 启动停止 / 打开主窗口 / 退出）
//   - 把 Service 注册为前端可调用的绑定（frontend/src/lib/api.ts 消费）

import (
	"embed"
	"log"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/icons"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	svc := newService()

	app := application.New(application.Options{
		Name:        "warp-go",
		Description: "Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3，mixed HTTP+SOCKS5）",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyRegular,
		},
	})

	// 主窗口：900x600 起，可缩放；前端 Tailwind 响应式适配宽窄屏。
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "warp-go",
		Width:     960,
		Height:    680,
		MinWidth:  720,
		MinHeight: 520,
		URL:       "/",
	})

	// 系统托盘：状态菜单 + 快速开关 + 退出。
	tray := app.SystemTray.New()
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(icons.SystrayMacTemplate)
	} else {
		tray.SetDarkModeIcon(icons.SystrayDark)
		tray.SetIcon(icons.SystrayLight)
	}

	menu := app.Menu.New()
	menu.Add("打开主窗口").OnClick(func(*application.Context) {
		window.Show()
	})
	menu.Add("启动代理").OnClick(func(*application.Context) {
		if err := svc.Start(); err != nil {
			log.Printf("启动失败：%v", err)
			window.Show()
		}
	})
	menu.Add("停止代理").OnClick(func(*application.Context) {
		if err := svc.Stop(); err != nil {
			log.Printf("停止失败：%v", err)
		}
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) {
		_ = svc.Stop()
		// 先显式关闭窗口再退出：托盘 AttachWindow 后仅 app.Quit() 在部分
		// 平台（尤其 Linux GTK）不会销毁已附加的主窗口，导致"退出无效，
		// 主窗口还在"。
		window.Close()
		app.Quit()
	})
	tray.SetMenu(menu)

	// 主窗口关闭时隐藏到托盘（而非退出），符合桌面代理应用习惯。
	tray.AttachWindow(window).WindowOffset(5)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
