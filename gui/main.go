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
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
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

	// 关闭按钮 → 最小化到托盘（而非退出程序）。WindowClosing 事件在窗口
	// 真正销毁前触发，hook 里 Cancel() + Hide() 即可拦截关闭、保留进程与
	// 托盘。真正退出走托盘菜单的 app.Quit()（不触发 WindowClosing）。
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if runtime.GOOS == "darwin" {
			// macOS 约定：关闭窗口即退出（与系统行为一致），不藏托盘。
			return
		}
		e.Cancel()
		window.Hide()
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
		// Stop 是异步信号（Server.Stop 只置位 + 关 stopCh），shutdown 在
		// Start 的 select 醒来后执行（含清除系统代理）。短暂等待保证
		// 退出前系统代理已同步关闭，避免 app.Quit 抢先终止进程留下残留。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && svc.IsRunning() {
			time.Sleep(50 * time.Millisecond)
		}
		// 直接 Quit（销毁 impl 结束进程）。不要调 window.Close()——
		// 它触发 WindowClosing 会被上面 hook 拦截成"隐藏到托盘"。
		app.Quit()
	})
	tray.SetMenu(menu)

	// 主窗口关闭时隐藏到托盘（而非退出），符合桌面代理应用习惯。
	tray.AttachWindow(window).WindowOffset(5)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
