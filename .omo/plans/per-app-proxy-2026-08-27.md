# 方案：warp-go 安卓版分应用代理（per-app proxy）

> 状态：**v1.0 调研定稿（2026-08-26）** — 待东哥/老马确认后排期实现
> 派单：东哥，多维表格 recvtp17nXI1QI | 前置：老马环境已备（CT107 模拟器就绪）
> 调研来源：AOSP 官方 Javadoc/API diff + WireGuard-android / v2rayNG / sing-box / CMFA 四项目源码逐行核实（commit 见 §5）

## 1. 目标与非目标

**目标**：Android 端支持分应用代理——「仅代理指定应用」（白名单）或「代理全部但排除指定应用」（黑名单）；默认行为 = 全量代理（现状不变）。桌面端不受影响。

**非目标**：按 URL/域名/进程的细粒度分流（已有 GEO 分流引擎负责 L7/L4 层）；iOS。

## 2. 分层定位：per-app 与 GEO 正交

```
┌─ VpnService 层 ────────────────────────────────┐
│ per-app 过滤：addAllowedApplication /          │  ← 本方案新增
│ addDisallowedApplication（内核级，包名粒度，   │
│ 不进 TUN 的应用流量根本不经过 sing-tun）        │
├─ Go TUN 栈层（androidvpn/ + decision.go）──────┤
│ GEO 分流：geoip:cn/geosite:cn → direct，       │  ← 已有，不动
│ 其余 → WARP 隧道                               │
└────────────────────────────────────────────────┘
```

两层叠加生效：先被 VpnService 排除的应用完全绕开隧道；进入 TUN 的流量再走 GEO 规则。**Go TUN 栈（androidvpn/、core/）零改动**——这是本方案最大的成本优势。

## 3. 现状代码事实（2026-08-26 @ 8d9d3b6 核实）

### 3.1 调用链

```
React UI (gui/frontend/src/lib/api.ts svc.Start())
  → Wails 绑定 gui/service.go Service.Start()
    → androidRequestVpnStart() [gui/androidbridge.go, 反向 JNI]
      → MainActivity.requestStartVpn() [MainActivity.java:78]
        → connectVpn():398 → VpnService.prepare() → startVpnService():411
          → startForegroundService(intent) 
            → WarpVpnService.onStartCommand():160   ★ per-app 插入点所在
              → Builder 配置区 :202-215 → establish():219
                → nativeStartVpn(fd, physicalDns) → Go sing-tun 栈
```

### 3.2 可复用的既有模式

| 需求 | 先例 | 位置 |
|---|---|---|
| 启动参数传 VPN 服务 | Intent extras（EXTRA_ASSIGNED_IPV4/6） | WarpVpnService.java:54-58 |
| 服务侧沙箱文件兜底读取 | readAssignedAddrs() 读 reg.json | WarpVpnService.java:353 |
| Go→Java 反向 JNI 新方法 | openExternalBrowser / exportDebugDiag | androidbridge.go:59-104 |
| 设置项持久化 | core.Config (config.json) + SetAutostart 模式 | gui/service.go:534, core/core.go:1013 |
| Manifest `<queries>` 声明 | 相机 intent queries | AndroidManifest.xml:21-28 |
| 设置页 UI Card | `<Card title>` + Toggle/Button/Field | SettingsPage.tsx |

## 4. 技术要点与设计决策（已按官方文档 + 业界对照校准）

### 4.1 VpnService API 语义（官方文档/AOSP Javadoc 已核实）

来源：AOSP frameworks/base `VpnService.java` / `services/core/.../Vpn.java`（main）、官方 API diff 归档。

| 事实 | 结论 |
|---|---|
| 最低 API level | **API 21**（addAllowed/addDisallowed 均为 Android 5.0 新增；项目 minSdk 21 恰好覆盖） |
| 互斥性 | 同一 Builder 只能有一组 allow 或一组 disallow；混用**在第二个调用处同步抛 UnsupportedOperationException** |
| 未安装包名 | add 调用时 `verifyApp()` 校验并抛 NameNotFoundException（受检异常，需 catch）；system_server 对未知包静默跳过 |
| 生效时机 | 列表是 Builder 配置一部分，**establish() 时一次性下发生效**（存入 system_server mConfig → NetworkCapabilities UIDs） |
| 运行中热更 | **无公开 API**。establish 后可变的只有 addAddress/removeAddress/setUnderlyingNetworks；改应用列表必须重建 Builder 重新 establish() |
| 重连语义 | 应用列表变化时系统禁止原地 handover（"Handover not possible due to changes to allowed/denied apps"）→ 强制新建 NetworkAgent；但交接顺序为**先建新接口、成功后再停旧接口**（官方 establish() 文档明示） |
| 再次授权 | `prepare()` 在用户同意过一次后返回 null——**变更应用列表重启 VPN 不会再弹授权框**；仅权限被撤销后才需重弹 |
| 自环 | 官方无"必须排除自身"明文；lockdown/always-on 模式系统自动豁免 VPN 自身 UID，普通 per-app 模式无此记载 [unverified] → 防御性自排除仍必要 |

### 4.2 自环防护（✅ 已按业界对照校准，修正初稿方向）

**推荐路线（与 WireGuard / sing-box / CMFA 三家一致）：壳自身始终留在隧道内，防环靠现有 `protectSocket(fd)`**：

- **黑名单模式**：apply 前 `disallow列表 − com.wails.app`（防止用户误把本应用加进排除集导致壳流量改道物理网——GitHub 下载/GEO 更新在国内直连会失败，属行为退化）
- **白名单模式**：apply 前 `allow列表 + com.wails.app`（保证壳自身流量维持现状走隧道）
- **UI 层双保险**：应用选择器直接剔除自身包名（sing-box PerAppProxyScreen L275 同法），用户根本选不到
- **为什么不用 v2rayNG 路线**（永远 addDisallowedApplication(自身)）：那会把壳全部流量（Go 核心 DoH/GEO/更新下载、WebView）改到物理直连，在国内网络下 GitHub 类请求必挂——是相对 v0.5.31 现状的行为退化；且 warp-go 的 protectSocket 防环已经真机验证（QUIC 重连稳定化 b99bcd1 等）
- 现状全量代理模式 = 自己在隧道内 + protect，per-app 只是给其他应用套同样的过滤，壳行为不变

### 4.3 配置数据模型（拟）

```jsonc
// core.Config 新增（config.json 持久化；桌面端忽略）
{
  "per_app_mode": "off",     // off=全量(默认) | allow=白名单 | disallow=黑名单
  "per_app_packages": []     // 生效的包名列表（allow/disallow 模式下使用）
}
```

### 4.4 传递路径（拟）：沙箱文件而非 extras
Go 侧启动前把 `{mode, packages[]}` 写入 `getFilesDir()/perapp.json`；WarpVpnService.onStartCommand 在 establish() 前读取（readAssignedAddrs 同模式）。理由：不依赖 Activity 存活（START_STICKY 重投也能拿到）、extras 有 1MB TransactionTooLarge 风险、文件可校验。

### 4.5 应用列表枚举（Java 侧新反向 JNI 方法）
`listInstalledApps()` → JSON 数组 [{package,label,system}]，供前端选择器使用；需 Manifest `<queries>` 扩展。

**包可见性结论（官方已核实）**：API 30+ 默认过滤 `getInstalledPackages()` 结果；标准解法是 `<queries>` 声明。QUERY_ALL_PACKAGES 属"极少数情况"兜底，且 Play 政策许可用途列表不含 VPN 类应用——本项目 sideload 分发不受 Play 审核约束，但为规范起见**优先 `<queries>` 路线**：
- 首选：`<intent><action android:name="android.intent.action.MAIN"/><category android:name="android.intent.category.LAUNCHER"/></intent>` —— 声明"可查询有启动器入口的应用"，恰好就是分应用选择器要列的集合（无 launcher 入口的纯后台/系统组件本就不该出现在选择器里），一箭双雕
- 该 intent 声明对 API 30+ 生效，低版本自动忽略（默认全可见），无需版本分支

### 4.6 UI 草案（沿用 SettingsPage Card 模式）

设置页新增 `<Card title="分应用代理">`（仅 Android 显示；桌面端 `platform` 判断隐藏）：

```
┌─ 分应用代理 ────────────────────────────────────────┐
│ 代理范围   ( ) 全部应用（默认）                      │
│            ( ) 仅指定应用（白名单）                  │
│            ( ) 排除指定应用（黑名单）                │
│                                                     │
│ [选择应用…]  ← 仅白/黑名单模式展开                   │
│ ┌──────────────────────────────┐                    │
│ │ 🔍 搜索应用名/包名            │                    │
│ │ ☐ 已选 3 个 · [清空]          │                    │
│ │ ── 已选 ──                   │                    │
│ │ ☑ Telegram  (org.telegram…)  │                    │
│ │ ── 全部应用（系统应用折叠）── │                    │
│ │ ☐ Browser (com.android…)     │                    │
│ │ ▸ 系统应用 (23)               │ ← 默认折叠         │
│ └──────────────────────────────┘                    │
│ ⚠ 本应用自身始终不进入代理列表（防路由死锁）        │
│                                                     │
│ [保存并重连]  ← 变更后需重启 VPN 生效               │
│ ℹ 应用列表变更会短暂重连隧道，不影响授权           │
└─────────────────────────────────────────────────────┘
```

要点（✅ 已对照四家 UI 公约数校准）：
- 三态单选（off/allow/disallow）映射 `per_app_mode`；默认 off = 现状全量代理（对应 CMFA 的 AcceptAll 默认语义）
- 选择器数据源：`listInstalledApps()` 反向 JNI，一次拉取前端缓存；**只列声明 INTERNET 权限的应用**（WireGuard/CMFA 同法，天然滤掉无网系统组件）；按「已选置顶 / 第三方 / 系统（默认折叠）」分组 + 搜索框
- com.wails.app 从可选列表剔除（sing-box L275 同法）+ 底部固定说明；apply 层再兜底（§4.2）
- 「保存并重连」= 写 config.json + 若 VPN 在运行则 stop→start（CMFA 等价做法；官方已确认不重弹授权）；未运行则只保存，下次启动生效
- 失效包名（app 已卸载）：加载配置时过滤掉并在 UI 标注"n 个已失效"
- 本期不做（留扩展位）：剪贴板导入导出、"扫描中国应用"、Shizuku 查询、排序自定义

## 5. 开源实现对照（基于各项目源码逐行核实，2026-08-26）

### 5.1 WireGuard-android（官方 app，commit e7b3a3c）

- **配置字段**：wg-quick 属性 `ExcludedApplications = pkg1, pkg2` / `IncludedApplications = ...`，互斥——两者同时非空时 `Interface.Builder.build()` 抛 BadConfigException（[Interface.java](https://github.com/wireguard/wireguard-android/blob/master/tunnel/src/main/java/com/wireguard/config/Interface.java) L241-244/L317-320）
- **数据流**：UI（AppListDialogFragment）→ 整条隧道存为一个 `.conf` 纯文本 → GoBackend.setState() 把 config 传给 wireguard-go；**ExcludedApplications 不进 Go 字符串**，纯 Android 层概念 → [GoBackend.java L300-305](https://github.com/wireguard/wireguard-android/blob/master/tunnel/src/main/java/com/wireguard/android/backend/GoBackend.java) 循环调 addDisallowed/addAllowed
- **自排除方式**：不在 Builder 排除自己；防环靠 socket protect（GoBackend L349-350 对 wg UDP socket 调 protect()）——与我们现有 protectSocket 思路同源，但**不覆盖 WebView 等壳自身流量**
- **运行中修改**：无热更；setState 内部先 DOWN 再 UP（失败回滚旧配置）= 重建隧道
- **UI 实践**：Material 对话框 + TabLayout「Exclude/Include」双 Tab 同列表切语义；只列声明了 INTERNET 权限的应用（getPackagesHoldingPermissions）；全选/反选按钮；按名称排序；**无搜索框**

### 5.2 v2rayNG（commit a1b45bb）

- **存储**：MMKV 键值——`pref_per_app_proxy`(bool 总开关) + `pref_per_app_proxy_set`(stringSet 包名) + `pref_bypass_apps`(bool：true=黑名单 bypass / false=白名单 proxy)（[AppConfig.kt](https://github.com/2dust/v2rayNG/blob/master/V2rayNG/app/src/main/java/com/v2ray/ang/AppConfig.kt) L25-27）
- **数据流**：Compose PerAppProxyActivity 增删勾选即写 MMKV → 服务侧 [CoreVpnService.configurePerAppProxy() L266-298](https://github.com/2dust/v2rayNG/blob/master/V2rayNG/app/src/main/java/com/v2ray/ang/service/CoreVpnService.kt) 从 MMKV 读回循环 add；xray/tun2socks 核心**完全不感知**分应用列表
- **自排除（四家中最显式）**：未开启或列表为空时也强制 `addDisallowedApplication(selfPackageName)`；bypass 模式把自身加进排除集、proxy 模式从 allow 集移除自身——"永远不允许自己进隧道"，与 WireGuard 的 protect 思路相反
- **运行中修改**：改列表 → `SettingsChangeManager.makeRestartService()` 置 AtomicBoolean 标记 → 返回主页时消费标记整体重启服务；无热更
- **UI 实践**：顶栏搜索框按名称/包名实时过滤；排序=已选中置顶 → 非系统应用在前；全选/反选

### 5.3 sing-box for Android（commit 3fd708f）

- **配置键名**：tun inbound JSON `include_package` / `exclude_package`（string 数组，仅 Android 且要求 auto_route；[官方文档](https://sing-box.sagernet.org/configuration/inbound/tun/)）
- **数据流**：UI 内部以 UID 集合做勾选状态、保存转回包名 → Jetpack DataStore（`per_app_proxy_enabled` / `per_app_proxy_mode`(int：EXCLUDE=1 默认/INCLUDE=2) / `per_app_proxy_list`）→ 启动时构造 `Libbox.OverrideOptions{includePackage/excludePackage}` 注入 Go 核心 → [VPNService.kt openTun() L141-163](https://github.com/SagerNet/sing-box-for-android/blob/main/app/src/main/java/io/nekohasekai/sfa/bg/VPNService.kt) 循环调 addAllowed/addDisallowed，逐个 catch NameNotFoundException
- **自排除**："自己留在隧道内"路线：include 模式 = 列表 + 自身包名；exclude 模式 = 列表 − 自身包名（BoxService.kt L143-149）；防环靠 protect(fd)；UI 层把自己从可选列表剔除（PerAppProxyScreen L275）
- **运行中修改**：半热更——保存后弹 Snackbar「reload required」，用户确认走 libbox serviceReload()（本质仍重走 openTun）
- **UI 实践（四家最丰富）**：搜索开关、排序模式+倒序、三档过滤（隐藏系统/无网络权限/禁用应用）、全选/清空、剪贴板导入导出、"扫描中国应用"自动生成托管黑名单、Shizuku/Root 特权查询包列表应对 API 30+ 可见性限制、AppChangeReceiver 监听安装卸载

### 5.4 Clash Meta for Android（commit c454d0e）

- **模式字段**：枚举 `AcceptAll / AcceptSelected / DenySelected`（默认 AcceptAll = 不过滤）；DataStore 存 `access_control_mode`(enum) + `access_control_packages`(stringSet)
- **消费位置**：service 进程 [TunService.kt TunModule.open() L155-168](https://github.com/MetaCubeX/ClashMetaForAndroid/blob/master/service/src/main/java/com/github/kr328/clash/service/TunService.kt)：AcceptSelected → `(packages + packageName).forEach { runCatching { addAllowedApplication(it) } }`；DenySelected → `(packages - packageName).forEach { runCatching { addDisallowedApplication(it) } }`
- **自排除**：同 sing-box"留在隧道内"路线——白名单 +自身、黑名单 −自身；UI 过滤掉自身
- **运行中修改**：整体重启（stop → 200ms 轮询等停干净 → start）
- **UI 实践**：搜索对话框、全选/全不选/反选/剪贴板导入导出、已选中置顶+可切换排序字段、系统应用默认隐藏可开关显示；列表只收声明 INTERNET 权限的应用（+uid<10000 系统 uid）

### 5.5 四项目横向对照表

| 维度 | WireGuard-android | v2rayNG | sing-box for Android | ClashMetaForAndroid |
|---|---|---|---|---|
| 存储格式 | wg-quick `.conf` 属性行 | MMKV：bool 开关+stringSet 包名+bool 模式 | DataStore stringSet 包名+int 模式 | DataStore enum 模式+stringSet 包名 |
| 白名单表达 | `IncludedApplications` | `PREF_BYPASS_APPS=false` | `include_package` / INCLUDE(2) | `AcceptSelected` |
| 黑名单表达 | `ExcludedApplications`（互斥 build 时校验） | `PREF_BYPASS_APPS=true` | `exclude_package` / EXCLUDE(1) 默认 | `DenySelected`（默认 AcceptAll） |
| add 调用位置 | GoBackend.java L300-305 | CoreVpnService.kt L290/L293 | VPNService.kt L145/L158 | TunService.kt L161/L166 |
| 自排除方式 | 不排自己；protect() 保护 wg socket | **addDisallowed(自身)** 三分支强制排除 | 留自己在隧道内：include +自身 / exclude −自身；protect(fd) 防环 | 同 sing-box：白名单 +自身 / 黑名单 −自身 |
| 变更生效 | 重建隧道（DOWN→UP 失败回滚） | 整体重启服务（标记消费制） | 用户确认 reload 或 restart | 整体重启（stop→轮询→start） |
| UI 搜索 | 无 | 有（名称/包名） | 有 | 有 |
| 排序/分组 | 仅字母序 | 已选置顶→非系统在前 | 排序模式+倒序可选 | 已选置顶+字段+倒序 |
| 系统应用 | 按 INTERNET 权限天然过滤 | 排末尾不隐藏 | 三档过滤开关 | 默认隐藏可显示 |

### 5.6 共性结论（对 warp-go 直接可用的事实）

1. **执行点全部收敛**到 VpnService.Builder.addAllowed/addDisallowedApplication，核心引擎（wireguard-go/xray/sing-box core/clash core）均不感知包名列表——印证本方案"Go TUN 栈零改动"
2. **全部逐个 try-catch NameNotFoundException**——已卸载应用不致命，与官方 verifyApp 行为对应
3. **自排除两派**：v2rayNG 把自己踢出隧道（少数派）；WireGuard/sing-box/CMFA 三家把自己留在隧道内 + protect(fd) 防环。**warp-go 现状架构即后者**（protectSocket 已真机验证），采用多数派路线改动最小
4. **变更生效无一支持真热更**：最轻也是"用户确认后 reload"（本质重连）；warp-go 采用 stop→start 与 CMFA 等价且实现更简单
5. **UI 公约数**：搜索框 + 已选置顶 + 系统应用默认隐藏/折叠 + 全选反选 + INTERNET 权限过滤；进阶件（导入导出/扫描中国应用/Shizuku）本期不做，留扩展位

## 6. 改动文件清单（预估）

| 文件 | 改动 |
|---|---|
| core/core.go | Config 加 PerAppMode/PerAppPackages 字段 + 默认值 + 持久化校验 |
| gui/service.go | 绑定 Get/SetPerAppConfig（SetPerApp 内 Android 分支写沙箱 perapp.json） |
| gui/androidbridge.go | 新增 listInstalledApps 反向 JNI 封装（C preamble 加方法 ID 缓存）+ Go 侧返回 JSON 给前端 |
| WarpVpnService.java | onStartCommand 读 perapp.json → establish() 前 apply allow/disallow（逐包 try-catch NameNotFoundException + 自身包名增删兜底） |
| MainActivity.java | listInstalledApps() 静态方法（getInstalledPackages + INTERNET 权限过滤 + label/system 标记） |
| AndroidManifest.xml | `<queries>` 加 MAIN/LAUNCHER intent-filter |
| gui/frontend SettingsPage.tsx | 新增「分应用代理」Card（仅 Android 显示）；新 PerAppPicker 组件（搜索/分组/勾选） |

## 7. 风险点

1. ~~运行中变更语义~~ → **已定论**（官方）：无热更 API；变更 = 重新 establish，系统强制新建 NetworkAgent 但先建新后拆旧、且不重弹授权框。UI 决策：保存列表后提示"将短暂重连以应用变更"，自动执行 stop→start。
2. **白名单模式下 DNS 行为**：per-app 过滤在 NetworkCapabilities UIDs 层实现 → 非成员应用根本看不到 VPN 网络，`addDnsServer(198.18.0.1)` 理应只作用于成员应用的 VPN 网络 [unverified 推理]——模拟器验收场景 3 显式验证非成员 app 的 DNS 正常性
3. ~~包可见性~~ → **路线已定**：`<queries>` + LAUNCHER intent-filter（§4.5）；QUERY_ALL_PACKAGES 不用（Play 政策不含 VPN 类，避免未来上架障碍）
4. **厂商魔改**：OPPO/PKG110 真机上 per-app 行为差异——测试计划覆盖真机
5. **START_STICKY 重投**：复活时 perapp.json 缺失/损坏 → 必须回退全量代理（fail-open 到现状行为），禁止 fail-closed 断网
6. **未安装包名**：Java add 时抛 NameNotFoundException（受检异常）——apply 列表前逐包 try/catch 跳过失效包名并记日志（用户卸载了被选中的 app 是常态）

## 8. 测试计划（拟）

- 单测：core.Config 序列化往返；Go 侧无 Android 依赖部分照常 CI
- CI 门（AGENTS.md §6.8）：本地 `GOOS=android CGO_ENABLED=0 go build ./...` 是假门（不编译 androidbridge.go cgo）→ 必须 push main + tag 并行构建，看 build-android job 全绿
- 模拟器验收（CT107 redroid11，技能 warp-go-android-debug 流程）：
  1. 默认 off：行为与 v0.5.31 一致（回归门）
  2. 黑名单排除某 app（如 WebView Browser Tester）：该 app 直连、其余进 tun（ip rule/tun0 采样 + ping 对照）
  3. 白名单仅含一个测试 app：其余 app 直连
  4. 运行中改列表 → 重启 VPN 后生效
  5. perapp.json 损坏 → 回退全量代理
- 真机（PKG110）复验 1-3 场景

## 9. 结论与下一步

**推荐方案一句话**：在 `WarpVpnService.onStartCommand` 的 establish() 前，从沙箱 `perapp.json` 读 `{mode, packages}` 并 apply 到 Builder（白名单 addAllowed / 黑名单 addDisallowed，互斥由模式字段保证）；壳自身始终留在隧道内（黑名单减自身/白名单加自身）+ 现有 protectSocket 防环；配置存 core.Config 随 config.json 持久化；UI 用设置页 Card + 应用选择器；变更生效 = stop→start 重连（授权不重弹）。Go TUN 栈零改动。

- [x] 外部调研回收（官方 AOSP + 四项目源码对照）→ §5 已填全、§4 已校准
- [ ] **东哥/老马确认本方案 → 进入实现排期**（改动集中：2 个 Java 文件 + Manifest + core.Config 绑定 + 前端一个 Card，预估 1-2 个开发轮次）
