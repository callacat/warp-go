# v0.5.31 收尾记录（2026-08-26）

> AGENTS.md 写入被平台保护闸拦截（审批弹窗超时），本文件承接收尾信息。
> **并入完成（2026-08-26）**：要点已并入 AGENTS.md——§6.5 补 M9.14（阶段10 退役风暴 + 阶段11/12 国内延迟治理）、M9.15（GUI 两修复 + 断联真根因 + v0.5.31 发布闭环），§8 末尾补 4f9cc48 断联根因与 GUI 两修复两条事实，「未解决问题交接」标记已解决；CHANGELOG 校对定稿（v0.5.1 错标 Unreleased 段转正、删空 Unreleased 段）。

## 本版闭环内容

1. **GUI 两修复**（东哥 08-26 派单，recvtkxq）：
   - 夜间主题启动不持久 → `ThemeContext` 根组件唯一实例 + index.html 预上色内联脚本（64300aa）
   - GEO 自动更新永不触发 → `InitDefaults` 到期补跑 + `geoAutoUpdateLoop` mtime 跨进程累计（dda7587）
2. **断联根因修复**（recvtdS6）：每请求杀连接——本地取消尾流误判连接级错误致共享 H3 连接按请求节奏被拆（4f9cc48，qlog 实验实锤：1730/1740ms 死亡时间与 CF edge 固定清理阈值一致）
3. **发布**：CHANGELOG 定版 [v0.5.31] - 2026-08-26，commit f82b4e9，tag v0.5.31
4. **CI 验证**：Build and Release(32920531770) ✅ + Docker GHCR tag(32920531822) ✅ + Docker GHCR main(32920531804) ✅ 全部 completed success
5. **Release 产物核验**：app-release.apk + app-release.aab + 5 平台 CLI + 3 平台 GUI 共 10 个 assets 齐全
6. **部署与验收（三端）**：
   - 主路由 ImmortalWrt：md5 8804b466 与产物一致，PID 运行正常（07:31）
   - CT103：warp-go.service active，隧道建立正常无 EOF 循环
   - Android APK：东哥真机覆盖安装实测"基本没问题"，验收通过
7. **任务状态**：recvtdS6 已关单（多维表格），recvtkxq 待回填后关单

## 过程坑（供后续参考）

- **双线并发**：task-relay 线与 feishu-group 群聊线同时推进同一任务（群聊线残留旧上下文自行跟进）。本次同动作未造成实际冲突（tag 仅一条、main 无分叉），但确认了方案B派单纪律的必要性：任务正文只走表格+relay，群聊仅知会。
- relay cron 大量 TimeoutError 重试堆积（桥忙时 */15 扫描反复超时），幂等设计防住了重复投递。

## 环境清理（2026-08-26 执行）

- 停止观测进程：qlog-pull.sh、traffic-gen.sh
- 删除工作树临时产物：dist-*（约 350MB）、.claude-task*、.tasks/、.codery-relay/qlog 归档
- GitHub workflow runs 清理（保留最新）；旧 release 处理见执行记录
