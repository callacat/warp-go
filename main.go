package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"warp/core"
)

// geoUpdateOneShotTimeout bounds a single -geo-update run. Each file download
// already carries its own 5-minute request timeout, so this only caps the
// whole operation.
const geoUpdateOneShotTimeout = 10 * time.Minute

// usage replaces the flag package's default listing. That listing sorts
// alphabetically, which puts the destructive -del first and buries -l, and it
// has nowhere to put the two caveats a new user most needs to know: UDP does not
// traverse the tunnel, and the default listen address is world-reachable with no
// authentication.
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `warp —— Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3，mixed HTTP+SOCKS5 前端）

用法：
  warp [选项]

代理：
  -l <host:port>   mixed HTTP+SOCKS5 监听地址（默认 127.0.0.1:40000，可被 config.json 覆盖）
  -user <用户名>   SOCKS5（RFC 1929）/ HTTP Basic 认证用户名；必须同时给出 -user 和 -pass 才启用认证
  -pass <密码>     认证密码
  -ip <取值>       连接哪个边缘（默认 4）：
                     4            注册信息中的 IPv4 边缘
                     6            注册信息中的 IPv6 边缘
                     <host:port>  改为连接指定地址，例如 162.159.198.2:4500、
                                  [2606:4700:103::2]:443、example.com:443
                   它决定的是"如何到达边缘"，不限制隧道内能访问什么 —— 目标
                   由边缘代为连接，所以走 IPv4 边缘一样能访问 IPv6-only 站点。
                   取 4 或 6 时会遍历注册给出的整个端口列表；给显式 host:port
                   时只使用该端口。域名由系统解析器解析（此时隧道尚未建立），
                   解析出的每个地址都会作为候选。

配置（config.json，位于执行目录；优先级：旗标 > config.json > 默认值）：
  -config <路径>   配置文件路径（默认 config.json；缺失时自动生成默认配置模板）
  -route <路径>    路由规则文件路径，覆盖 config.json 的 rules_path
  -sysproxy        启用系统代理，覆盖 config.json 的 enable_system_proxy
  -geo-update      立即更新 GEO 数据（geosite/geoip）后退出

  监听地址、GEO 仓库与自动更新周期、UDP 开关、系统代理开关等由 config.json
  控制，文件变更热重载（rules_path / geo_dir / enable_system_proxy 即时生效，
  其余需重启）。rules.txt 是路由规则（每行"行为,条件"，行为 proxy/direct），
  编辑保存即热重载。

注册：
  -reg             尚未注册时执行注册，然后退出
  -del             向 API 注销并删除本地注册信息

注册信息保存在工作目录下的 reg.json。首次使用需先注册：启动本身从不注册，
因为创建账号是一个需要明确表达的动作。-reg 是幂等的 —— 已有注册时只报告并
退出，而不是替换掉它；替换会让旧注册在 Cloudflare 侧失去本地凭据，再也无法
注销。要更换注册，请先用 -del。

边缘地址与端口列表来自注册信息，因此没有单独的端点参数；端口按 API 返回的
顺序尝试，并从上次成功的那个开始。

示例：
  warp -reg                               注册（首次使用）
  warp                                    用已保存的注册信息运行
  warp -ip 6                              通过 IPv6 连接边缘
  warp -ip 162.159.198.2:4500             指定边缘地址与端口
  warp -ip example.com:443                通过域名连接自定义边缘
  warp -l 127.0.0.1:1080                  只监听回环地址
  warp -l 0.0.0.0:1080 -user u -pass s    对外提供服务并要求认证
  warp -del && warp -reg                  更换注册

扫描（可选，默认关闭）：
  扫描用与正式连接同一协议栈的轻量 QUIC 握手探针，对 WARP 边缘全段测真实往返
  延迟，按 RTT 升序取最低的 N 个端点前置到候选列表；注册端点作兜底尾接。不修改
  reg.json，失败回退到注册端点，不致命。-ip 4 扫 IPv4 段、-ip 6 扫 IPv6 段；
  -ip <host:port> 指定显式端点时扫描被忽略（显式端点优先）。详见用法末尾。

  warp -scan                              扫描并选用最低延迟的 IPv4 端点
  warp -scan -ip 6                        扫描 IPv6 段
  warp -scan -scan-ports 443              只扫 443 端口（更快覆盖更广）
  warp -scan -scan-top 8                  选用 8 个端点而非默认 4

参数：
  -scan                    启动前扫描 WARP 边缘全段并选用最低延迟的端点
  -scan-cidr <cidr,...>    追加自定义 CIDR 到默认段（默认 5 个 WARP IPv4 段或 2 个 IPv6 段）
  -scan-ports <p,p,...>    覆盖扫描端口（空则读 reg.json 的 endpoint_ports）
  -scan-concurrency <n>    并发探针数（0=自动 min(64, NumCPU*8)，下限 16）
  -scan-timeout <dur>      扫描总超时（默认 45s）
  -scan-per-probe <dur>    单探针超时（默认 3s）
  -scan-top <n>            选用 RTT 最低的 N 个端点前置（默认 4）

注意：
  UDP ASSOCIATE 的数据报不经过 WARP 隧道。plain CONNECT 是字节流隧道，无法
  承载数据报，因此它们从本机网络栈直接发出，对端看到的是你的真实地址。
  TCP 走隧道，UDP 不走。

  默认监听 127.0.0.1:40000（config.json 可改）。若改绑 0.0.0.0 对外提供
  服务，请务必设置 -user 与 -pass 认证。

`)
}

func main() {
	var (
		listen = flag.String("l", "", "mixed HTTP+SOCKS5 监听地址 host:port（默认 127.0.0.1:40000，可被 config.json 覆盖）")
		user   = flag.String("user", "", "SOCKS5/HTTP 认证用户名（与 -pass 同时给出才启用认证）")
		pass   = flag.String("pass", "", "SOCKS5/HTTP 认证密码（与 -user 同时给出才启用认证）")
		ip     = flag.String("ip", "4", "WARP 边缘：4、6，或显式 host:port")
		reg    = flag.Bool("reg", false, "尚未注册时执行注册，然后退出")
		del    = flag.Bool("del", false, "向 API 注销并删除本地注册信息")

		configPath   = flag.String("config", "config.json", "配置文件路径（缺失时自动生成默认配置）")
		routeFlag    = flag.String("route", "", "路由规则文件路径（覆盖 config.json 的 rules_path）")
		sysProxyFlag = flag.Bool("sysproxy", false, "启用系统代理（覆盖 config.json 的 enable_system_proxy）")
		geoUpdate    = flag.Bool("geo-update", false, "立即更新 GEO 数据后退出")

		// 扫描（可选，默认关闭）：启动前对 WARP 边缘全段做真实 QUIC 握手探针，
		// 按 RTT 升序取 top-N 端点前置到 edgeAddrs，注册端点作兜底尾接。失败回退
		// 到注册端点，不致命（与方案 §5.4 一致：不写回 reg.json，保留 -reg/-del 幂等）。
		scan         = flag.Bool("scan", false, "启动前扫描 WARP 边缘全段并选用最低延迟的端点")
		scanCidr     = flag.String("scan-cidr", "", "逗号分隔，追加自定义 CIDR 到默认段（4 或 6）")
		scanPorts    = flag.String("scan-ports", "", "覆盖扫描端口（空则读 reg.json 的 endpoint_ports）")
		scanConc     = flag.Int("scan-concurrency", 0, "并发探针数（0 为自动 min(64, NumCPU*8)，下限 16）")
		scanTimeout  = flag.Duration("scan-timeout", 45*time.Second, "扫描总超时（硬上限）")
		scanPerProbe = flag.Duration("scan-per-probe", 3*time.Second, "单探针超时")
		scanTop      = flag.Int("scan-top", 4, "选用 RTT 最低的 N 个端点前置")
	)
	flag.Usage = usage
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// -sysproxy 是三态覆盖：旗标给出时强制（true/false），未给出时按
	// config.json 的 enable_system_proxy（nil）。
	var sysProxyOverride *bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "sysproxy" {
			v := *sysProxyFlag
			sysProxyOverride = &v
		}
	})

	// 组装可复用核心。注册/注销/GEO 更新只用到其中一部分能力，Start 之前
	// 这些操作都不触碰网络代理。
	srv := core.New(core.Options{
		ConfigPath:      *configPath,
		ListenAddr:      *listen,
		Username:        *user,
		Password:        *pass,
		EdgeIP:          *ip,
		RulesPath:       *routeFlag,
		SysProxy:        sysProxyOverride,
		Scan:            *scan,
		ScanCIDR:        *scanCidr,
		ScanPorts:       *scanPorts,
		ScanConcurrency: *scanConc,
		ScanTimeout:     *scanTimeout,
		ScanPerProbe:    *scanPerProbe,
		ScanTop:         *scanTop,
	})

	// -del and -reg are administrative: each does its one job and exits, so that
	// registering cannot be confused with starting the proxy.
	if *del {
		log.Println("正在注销...")
		if err := srv.Deregister(); err != nil {
			log.Fatalf("注销失败：%v", err)
		}
		log.Println("✓ 注销成功")
		return
	}

	if *reg {
		// Registering is idempotent: an existing registration is left alone.
		// Replacing it silently would strand the old one on Cloudflare's side
		// with no local credential left to delete it.
		existing, id, err := srv.Register()
		if err != nil {
			log.Fatalf("注册失败：%v", err)
		}
		if existing {
			log.Printf("已注册：id=%s（reg.json）", id)
			log.Println("无需操作。要换一个注册，请先用 -del 注销。")
			return
		}
		log.Printf("✓ 注册信息已保存到 reg.json（id=%s）", id)
		log.Println("不带 -reg 运行即可启动代理。")
		return
	}

	// -geo-update：一次性更新 GEO 数据后退出。不需要注册信息，放在注册检查
	// 之后（与启动路径同用 core.UpdateGeo，config.json 缺失时自动生成模板）。
	// 先 InitDefaults 确保基础文件（config.json / 默认规则 / 缺失的 GEO），
	// 再 UpdateGeo 强制 SHA-1 比对刷新——首次使用一次命令完成全部初始化。
	if *geoUpdate {
		gctx, gcancel := context.WithTimeout(context.Background(), geoUpdateOneShotTimeout)
		defer gcancel()
		if err := srv.InitDefaults(gctx); err != nil {
			log.Fatalf("初始化基础文件失败：%v", err)
		}
		updated, uerr := srv.UpdateGeo(gctx)
		if uerr != nil {
			log.Fatalf("GEO 数据更新失败：%v", uerr)
		}
		if updated {
			log.Println("✓ GEO 数据已更新")
		} else {
			log.Println("✓ GEO 数据已是最新")
		}
		return
	}

	// 启动：config.json 加载、注册信息检查、边缘解析、MASQUE 连接、分流引擎、
	// mixed 代理、系统代理、GEO 自动更新与热重载全部在 core.Server 内部接线；
	// 此处只负责信号 → context 的转换。SIGINT/SIGTERM 触发优雅关停。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Start(ctx); err != nil {
		if errors.Is(err, core.ErrNoRegistration) {
			log.Fatal("reg.json 中没有注册信息。请先执行 warp -reg。")
		}
		log.Fatalf("启动失败：%v", err)
	}
}
