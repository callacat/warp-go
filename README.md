# warp-go

基于 Go 的 Cloudflare WARP 客户端，通过 **MASQUE over QUIC/HTTP-3** 建立隧道，前端以 **mixed HTTP+SOCKS5** 暴露，支持 **GEO 数据库分流**与**系统代理**。免 root、无 TUN、不改动路由——纯代理客户端实现，所有协议参数、注册流程、信任模型均对齐官方 `warp-svc`（详见 [`docs/warp-masque-reverse-engineering.md`](docs/warp-masque-reverse-engineering.md)）。

```
客户端 ──► mixed HTTP+SOCKS5 代理 ──► GEO 分流 ──► 隧道 / 直连
              （proxy 规则）                    │           │
                                                ▼           ▼
                                        tunnel/masque.go  本地直连
                                        （QUIC/H3 ─► WARP 边缘）
```

> [!WARNING]
> 默认监听 `127.0.0.1:40000`。若改为对外监听，**不要求认证**的默认配置会对局域网内任何地址开放代理。在不可信网络中请绑定回环地址或设置 `-user` / `-pass`。
>
> **UDP ASSOCIATE 的数据报不经过 WARP 隧道**——plain CONNECT 是字节流隧道，无法承载数据报，UDP 从本机网络栈直接发出，对端看到的是你的真实地址。TCP 走隧道，UDP 不走。

## 功能特性

- **mixed HTTP + SOCKS5 代理**：同一端口按首字节嗅探（`0x05` → SOCKS5，`CONNECT`/`GET` → HTTP），支持 CONNECT 隧道与绝对 URI 转发
- **GEO 数据库分流**：参考 Mihomo/Clash 规则语法，按 `geosite`（域名分类）与 `geoip`（IP 国家）匹配，规则在 **GUI 文本框自由增删改**，保存即热重载
- **系统代理**：一键把系统代理指向本地 mixed 端口（Windows 注册表 / macOS networksetup / Linux gsettings）
- **配置热重载**：`config.json` 与 `rules.txt` 文件变更自动生效，无需重启
- **GEO 自动更新**：默认从 `MetaCubeX/meta-rules-dat` 拉取，可设更新周期（默认 7 天）、手动触发、自定义仓库
- **GUI（Wails v3）**：系统托盘 + 状态 / 规则 / GEO / 设置 / 日志五页，独立可执行程序
- **Android（v0.5.0）**：Wails v3 Android 壳 + 自写 Java `VpnService`（TUN fd 经 JNI 交给 Go 内核），CLI/GUI/Android 三端共用 `core.Kernel`
- **边缘扫描**（`-scan`）：启动前对 WARP 边缘全段测 RTT，选用最低延迟端点

## 依赖

- Go ≥ 1.26.5（`go.mod` 锁定）
- GUI 额外：Node ≥ 20（前端构建）、Wails v3（`go install github.com/wailsapp/wails/v3/cmd/wails3@latest`）、Linux 需 `libgtk-4-dev libwebkitgtk-6.0-dev`

## 构建

```bash
# CLI
go build -o warp .

# GUI（前端 + 后端，产物 warp-gui）
cd gui
npm --prefix frontend ci && npm --prefix frontend run build
go build -o warp-gui .
```

## 快速开始

```bash
# 1) 首次使用：注册（注册信息保存在执行目录 reg.json）
./warp -reg

# 2) 启动代理（默认监听 127.0.0.1:40000，首次自动生成 config.json 与 rules.txt）
./warp

# 3) 用 curl 验证（走隧道）
curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace
# 若走 WARP，返回的 trace 中 warp=on

# 4) GUI 版（托盘 + 图形界面）
./warp-gui
```

## 命令行参数

```
warp —— Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3，mixed HTTP+SOCKS5 前端）

代理：
  -l <host:port>   mixed 监听地址（默认 127.0.0.1:40000，可被 config.json 覆盖）
  -user <用户名>   SOCKS5（RFC 1929）/ HTTP Basic 认证用户名；同时给出 -user 和 -pass 才启用
  -pass <密码>     认证密码
  -ip <取值>       连接哪个边缘（默认 4）：4 / 6 / <host:port>

配置（config.json，位于执行目录；优先级：旗标 > config.json > 默认值）：
  -config <路径>   配置文件路径（默认 config.json；缺失时自动生成默认模板）
  -route <路径>    路由规则文件路径，覆盖 config.json 的 rules_path
  -sysproxy        启用系统代理，覆盖 config.json 的 enable_system_proxy
  -geo-update      立即更新 GEO 数据（geosite/geoip）后退出

注册：
  -reg             尚未注册时执行注册，然后退出
  -del             向 API 注销并删除本地注册信息

扫描（可选，默认关闭）：
  -scan                启动前扫描 WARP 边缘全段并选用最低延迟的端点
  -scan-cidr <c,...>   追加自定义 CIDR 到默认段
  -scan-ports <p,...>  覆盖扫描端口
  -scan-concurrency <n> 并发探针数
  -scan-timeout <dur>  扫描总超时（默认 45s）
  -scan-per-probe <dur> 单探针超时（默认 3s）
  -scan-top <n>        选用 RTT 最低的 N 个端点前置（默认 4）
```

## 路由规则（rules.txt）

规则不是内置写死的：首次启动生成默认 `rules.txt` 作为模板，之后**可在 GUI 文本框自由增删改**，也可直接编辑文件（保存/变更即热重载）。

语法：每行一条 `行为,条件`；`#` 开头为注释，空行忽略。

```
# 默认模板
proxy,geosite:google
proxy,geosite:geolocation-!cn
direct,geoip:private
direct,geosite:private
direct,geosite:cn
direct,geoip:cn
```

| 条件 | 含义 | 例子 |
|---|---|---|
| `geosite:<name>` | 域名匹配 geosite 分类（后缀匹配，含子域） | `proxy,geosite:google` |
| `geoip:<cc>` | IP 国家匹配 | `direct,geoip:cn` |
| `geoip:private` | 内网/保留地址段（库内真实条目） | `direct,geoip:private` |
| `geoip:lan` | 回环/私有/链路本地（代码内置检查） | `direct,geoip:lan` |
| `domain:<suffix>` | 直接域名后缀匹配 | `proxy,domain:example.com` |

- 行为：`proxy`（走 WARP 隧道）/ `direct`（本地直连）
- 匹配顺序：**先命中先生效**（文件顺序）
- 未匹配 → 隐式 `direct` 兜底
- `geosite:geolocation-!cn` 是**字面类别名**（非中国站点），`!` 烘焙在数据库中

## 配置（config.json）

位于程序执行目录，首次启动自动生成默认模板，**文件变更热重载**。

```json
{
  "listen_addr": "127.0.0.1:40000",
  "rules_path": "rules.txt",
  "geo_dir": "geo",
  "geo_repo": "https://github.com/MetaCubeX/meta-rules-dat",
  "geo_auto_update_days": 7,
  "enable_system_proxy": false,
  "allow_udp": false,
  "download_proxy": "https://gh-proxy.org/"
}
```

| 字段 | 含义 |
|---|---|
| `listen_addr` | mixed 代理监听地址 |
| `rules_path` | 路由规则文件路径 |
| `geo_dir` | GEO 数据库目录（geosite.dat + geoip-lite.dat） |
| `geo_repo` | GEO 数据发布仓库（GUI 可编辑；下载 URL 由此推导） |
| `geo_auto_update_days` | GEO 自动更新间隔（天），0 关闭（GUI 可设） |
| `enable_system_proxy` | 启动时是否把系统代理指向本地端口 |
| `allow_udp` | 是否响应 SOCKS5 UDP ASSOCIATE（数据报直连，不经隧道） |
| `download_proxy` | GitHub 下载加速前缀（GUI 可编辑），仅对 github.com / raw.githubusercontent.com 的下载 URL 生效；置空关闭 |

优先级：**命令行旗标 > config.json > 默认值**。热重载基于 mtime + 内容 hash 检测。

## 注册信息（reg.json）

保存在**执行目录**（无路径参数），权限 `0600`。含设备 ID/token、ECDSA P-256 私钥、边缘公钥（证书固定）、端点地址与端口列表等。**不含**可导入 GUI 的密钥明文视图——`core.Status` 只暴露无密钥材料的安全快照。

> [!NOTE]
> 端点分配若变化，本项目不会自动刷新——需 `warp -del` 后重新 `warp -reg`。

## GUI

`warp-gui` 是独立的 Wails v3 桌面程序（React 19 + Vite + Tailwind v4 前端）：

- **系统托盘**：状态菜单、启动/停止代理、打开主窗口、退出
- **状态页**：运行状态、监听地址、分流统计（proxy/direct/miss 计数）
- **规则页**：路由规则文本框（行号、语法校验、保存即热重载）
- **GEO 页**：数据库状态、立即更新、自动更新周期、仓库地址
- **设置页**：完整配置表单（监听地址、GEO 仓库/URL、更新周期、UDP 开关）
- **日志页**：最近 500 条运行日志，级别着色

前后端边界是 **Wails 服务绑定**（`gui/service.go` → 前端 `frontend/src/lib/api.ts`），开发期前后端分离跑（Vite dev server + Go 进程），交付期 `//go:embed` 合一为单文件。

## Android（v0.5.2）

Android 版是 **Wails v3 Android 壳 + 自写 Java `VpnService`**（不使用 gomobile）：

- **架构**：`WarpVpnService.java` 用 `VpnService.Builder`（addAddress 填入 WARP 分配的 IPv4/IPv6、addRoute 全量路由、setMtu(1500)、setBlocking）`establish()` 拿到 TUN fd，经 JNI 传给 Go 侧 `gui/androidbridge.go` 的 `nativeStartVpn(fd)` / `nativeStopVpn()`（`//export Java_com_wails_app_WarpVpnService_*`，与 Wails 自带 18 个导出共存于同一 `libwails.so`）
- **共享内核**：隧道、GEO 分流、注册信息解析统一走 `core.Kernel`（桌面 CLI/GUI 同款），`androidvpn/` 的 sing-tun 栈把 TUN fd 接进 Go，分流决策（proxy/direct/reject）与桌面语义一致
- **启动/停止桥接 VpnService**：GUI 的"启动/停止"按钮经反向 JNI 桥（`MainActivity.requestStartVpn/requestStopVpn`）驱动 VpnService 生命周期——一键注册 → 启动 → consent → TUN 隧道，无需手动操作
- **运行时文件统一在应用沙箱**：GUI 服务层（`serverInstance`）与 JNI 侧（`buildAndroidConfig`）都把 config.json / reg.json / rules.txt / geo 锚定到 `getFilesDir()`（经 `core.Options.DataDir` + `gui/datadir_android.go`，首次成功缓存防桥接抖动），不再落到只读的 `/system/bin`
- **Consent**：`MainActivity` 首启调 `VpnService.prepare()` 请求授权（singleTask），授权后启动前台 `dataSync` 服务；包名保持 `com.wails.app`（JNI 符号烘焙进包名），用户可见名 "warp-go"
- **跟随系统主题**：前端三态主题（浅色/深色/跟随系统），经 Wails runtime `System.IsDarkMode()` + `android:ThemeChanged` 事件自动切换
- **状态栏不覆盖**：`WindowCompat.setDecorFitsSystemWindows(true)` 显式适配系统栏（Android 15+ 默认 edge-to-edge）
- **手机底部导航**：`<md` 隐藏侧边栏，底部固定导航（6 页 + 主题循环）
- **构建**：本地无 SDK/NDK/JDK → **仅 CI 构建**（`build-android` job：JDK 21 + SDK + NDK r27 → c-shared arm64/x86_64 + gradle APK/AAB + JNI 符号双侧 grep 断言）；产物从 Actions artifact 下载（`app-debug.apk`）

### Android 使用

1. 从 `build-android` job artifact 下载 `app-debug.apk` 安装（debug keystore 签名，可安装，非 Play 商店签名）
2. 首次启动会弹 VpnService 授权对话框 → 允许
3. **手动放入 reg.json**：WARP 注册信息需放入应用沙箱目录（`/data/data/com.wails.app/files/`，即 `getFilesDir()`）才能启动 VPN——可用 `adb push`（如 `adb push reg.json /sdcard/` 后用文件管理器/`adb shell run-as com.wails.app cp` 移入沙箱），或用桌面端 `./warp -reg` 生成的 reg.json
4. 打开 VPN 开关，`curl`/浏览器验证 `warp=on`

> [!NOTE]
> Android 版无真机验证（本机无设备/模拟器）——运行时行为需真机测试，验收项见 [`.omo/plans/warp-go-android-2026-08-01.md`](.omo/plans/warp-go-android-2026-08-01.md) §11。

## 项目结构

```
warp-go/
├── main.go                       # CLI 入口（flag 解析、调用 core.Server）
├── core/                         # 可复用核心：Server 生命周期、配置、注册、状态
│   ├── core.go                   #   Start/Stop/Status/SetSystemProxy/ReloadRules/SaveConfig
│   ├── kernel.go                 #   Kernel（MasqueClient+Engine+注册，CLI/GUI/Android 三端共用）
│   ├── config.go                 #   Config 结构 + config.json 加载/热重载
│   ├── status.go                 #   可序列化状态快照（GUI 轮询用）
│   └── register.go               #   注册/注销/注册信息视图
├── route/                        # GEO 分流引擎
│   ├── rules.go                  #   规则解析、默认模板、rules.txt 热重载
│   ├── geodata.go                #   geosite.dat / geoip-lite.dat 解析（v2ray protobuf）
│   ├── download.go               #   GEO 下载（SHA-1 去重 + proto 校验 + 原子写）
│   ├── matcher.go                #   匹配引擎（Engine.Match，first-match-wins）
│   └── engine.go                 #   引擎装配与生命周期
├── proxy/                        # mixed HTTP+SOCKS5 代理（首字节嗅探）
├── sysproxy/                     # 系统代理（Windows/macOS/Linux；android no-op stub）
├── autostart/                    # 开机自启（Windows/macOS/Linux；android no-op stub）
├── androidvpn/                   # (M7) TUN 栈（sing-tun）+ decision.go 决策逻辑（宿主可测）
├── gui/                          # Wails v3 GUI（main.go/service.go/logs.go + frontend/）
│   ├── androidbridge.go          #   Android JNI 桥（nativeStartVpn/nativeStopVpn）
│   └── build/android/            #   Android 工程（WarpVpnService.java + manifest + gradle）
├── registration/                 # 上游既有：两步注册 API
├── tunnel/                       # 上游既有 + RouteFunc 分流 + DialTunnel
├── scanner/                      # 上游既有：边缘延迟扫描（-scan）
├── .github/workflows/            # sync-upstream / build-release / docker-ghcr / build-android
├── AGENTS.md                     # 接手指南（架构、决策记录、验证方式）
└── docs/                         # 上游逆向文档
```

## 与官方 `warp-svc` 对齐情况

| 维度 | 官方 | warp-go | 说明 |
|---|---|---|---|
| 源连接 ID | 20 字节 | 20 字节 | 一致 |
| 流控窗口 | 10MB / 1MB | 10MB / 1MB | 一致 |
| 并发流上限 | 100 / 100 | 100 / 100 | 一致 |
| UDP 载荷上限 | 1350 | 1350 | 一致 |
| 边缘公钥固定 | PEM `bcmp` | `ecdsa.Equal` | 等价 |
| TLS 曲线 | PQ + P-256/384/521 | P-256/384/521 | PQ 组 Go 无法提供 |
| 代理 CONNECT | H3 only | H3 only | 一致 |
| DoH 传输 | H2 多路复用，单连接 | 同 | 一致 |
| DoH 上游 | 162.159.36.1 / 46.1 | 同 | 一致 |
| DoH 位置 | 隧道外（宿主解析器） | 隧道**内** | **有意分歧**（避免 DNS 泄漏） |
| TUN / Connect-IP | 有 | 无 | 超出项目范围 |
| 端点延迟优选 | 无 | `-scan` 手动触发 | **有意增强** |

## 已知限制

1. **QUIC/UDP 被完全封锁时无回退**——代理模式官方自身也是 H3-only。
2. **UDP 不走隧道且无法关闭**——数据报以真实源地址发出；需严格避免泄漏的场景应在上层限制客户端只用 TCP。
3. **规则仅作用 TCP CONNECT**——UDP 全直连（上游架构限制，需 TUN 才能改变，超出项目范围）。
4. **重连是惰性的**——空闲断线不会后台恢复，下一个请求承担重连延迟。
5. **PQ 密钥交换无法对齐**（Go 标准库无 `P256Kyber768Draft00`）。
6. **注册信息不会刷新**——端点一直沿用，需 `-del` 后重新 `-reg` 更新。
7. **Android 无真机验证**（v0.5.0-v0.5.2）——仅 CI 构建，运行时行为需真机测试；验收项见 android 计划文档 §11（TUN `warp=on`、consent UX、GEO 分流、Always-On 重启、JNI 无 `UnsatisfiedLinkError`、前台服务/电池等）。v0.5.1 修复 /system/bin 只读崩溃；v0.5.2 修复启动内核失败（桥接 VpnService）、日志不可见、注册状态切页丢失、扫描无候选。
8. **Android 需手动放入 reg.json**——注册信息不在 UI 内生成，需从桌面端 `-reg` 复制进沙箱 `getFilesDir()` 才能启动 VPN（v0.5.1 起注册写盘已正确落到沙箱，UI 内"一键注册"可用）。
9. **Android UI 为初版 consent 流**——首启自动弹授权对话框；React 前端按钮触发路径为后续版本。Wails v3 Android 仍 experimental（alpha2.119，已知 bug：onDestroy #5859、bindings 上下文 #5810 等）。

## 文档

- [CHANGELOG.md](CHANGELOG.md) — 版本变更记录
- [AGENTS.md](AGENTS.md) — 接手指南（架构、决策记录、构建/验证命令、GEO 格式定论）
- [docs/warp-masque-reverse-engineering.md](docs/warp-masque-reverse-engineering.md) — 官方 warp-svc 逆向分析
- [.omo/plans/warp-go-reinit-2026-07-31.md](.omo/plans/warp-go-reinit-2026-07-31.md) — 项目计划（随进度更新）
- [.omo/plans/warp-go-android-2026-08-01.md](.omo/plans/warp-go-android-2026-08-01.md) — v0.5.0 Android 里程碑计划与执行记录

## 更新记录

| 版本 | 日期 | 摘要 |
|---|---|---|
| [v0.5.3](CHANGELOG.md#v053---2026-08-02) | 2026-08-02 | Android cgo 编译修复、JNI 签名对齐（nativeBridgeReady void→int）、启动失败回滚（状态不再撒谎） |
| [v0.5.2](CHANGELOG.md#v052---2026-08-01) | 2026-08-01 | Android 内核启动根治（反向 JNI 桥接 VpnService）、日志 init 路由、注册状态切页弹性、扫描无候选修复、状态栏不覆盖、手机底部导航 |
| [v0.5.1](CHANGELOG.md#unreleased---v051-计划中) | 2026-08-01 | Android 运行时文件统一沙箱锚定（修复 /system/bin 只读崩溃）、日志时间戳去重、注销反馈、竖屏侧边栏自适应、跟随系统主题（全平台） |
| [v0.5.0](CHANGELOG.md#v050---2026-08-01) | 2026-08-01 | Android 版（Wails VpnService + JNI）、core.Kernel 三端复用、CI build-android、geoip/reject 修复 |
| [v0.4.0](CHANGELOG.md#v040---2026-08-01) | 2026-08-01 | REJECT 广告拦截、GitHub 下载加速（GUI 可配）、首启引导修复、GUI 多项修复 |
| [v0.3.1](CHANGELOG.md#v031---2026-07-31) | 2026-07-31 | Windows 修复、扫描独立页、开机自启 |
| [v0.2.0](CHANGELOG.md#v020---2026-07-31) | 2026-07-31 | 首个功能完整版本：GEO 分流、mixed 代理、系统代理、GUI |
| [v0.1.x](CHANGELOG.md#v010---2026-07-28) | 2026-07-28 | 首个可用版本 |

完整变更见 [CHANGELOG.md](CHANGELOG.md)。

## 许可证

（见仓库根目录 LICENSE 文件或遵循 Cloudflare WARP 相关服务条款；本项目为独立的第三方实现，非官方产品。）
