package tunnel

import "time"

// 拨号重试节奏（recvu2HKHM5zIj，2026-09-02 CT103 事故）：
//
// 旧逻辑两处拨号循环（装配 NewMasqueClientContext + 重连航班 runReconnect）
// 都是指数退避 100ms→5s 封顶、永不停止：边缘全端口不可达时每 5s 一轮、每轮
// 7+ 条「QUIC 拨号/边缘不可达」日志紧循环刷 journal——CT103 上 8-29 起对 CF
// 边缘 QUIC 全端口失败，journal 每 1~2 秒一条连刷 4 天（叠加每次重试写一个
// qlog 文件把 tmpfs 顶爆，引发宿主 IO 风暴）。
//
// 本策略按东哥要求改为「重试 3 次不通即停」：
//   - 一「轮」= 一次完整 dial（扫全部候选边缘地址，全失败记 1 轮）；
//   - 连续 dialRoundFailureLimit 轮全失败 → 停止密集重试，切入
//     retryLongBackoff 长退避（每 30 分钟低频探测一轮，期间完全静默）；
//   - 任一轮成功 → 计数清零，自动回到原有快速节奏（拆线瞬间尽快恢复）。
//
// journal 占用对比：旧 = 故障期每分钟 ~96 条、永不停止；新 = 密集期 3 轮
// 详细日志 + 1 条「进入长退避」+ 之后每 30 分钟 0 条，恢复时 1 条「已重建」。
const (
	// dialRoundFailureLimit 是连续全失败轮数阈值：达到即停止密集重试。
	dialRoundFailureLimit = 3

	// retryLongBackoff 是密集重试停止后的低频探测间隔。取 30 分钟：边缘
	// 级故障（路由/防火墙/CF 侧问题）的恢复以小时计，30 分钟一轮足以在
	// 恢复后半小时内自动接上，又不会对故障网络构成任何可感知压力。
	retryLongBackoff = 30 * time.Minute
)

// dialRetryPolicy 封装两个拨号循环共用的重试节奏（见包级常量注释）。非并发
// 安全：装配循环（构造期，无并发）与重连航班（singleflight 保证同时最多一
// 个）各自独享一个实例；成功即退出循环（实例随之丢弃），无需复位逻辑——
// 下一次拆线/装配自然从零开始。
type dialRetryPolicy struct {
	rounds    int  // 连续全失败轮数
	announced bool // 已打过「进入长退避」日志（每个故障期只报一次）
}

// afterFailure 在一轮拨号失败后调用，返回下一轮拨号前应等待的时长。
// firstBackoff 为 true 表示本次恰好切入长退避，调用方应打一条进入退避的
// 边界日志；此后 wait 等于 retryLongBackoff 的每一轮都属退避期，调用方应
// 静默（调用方以 wait >= retryLongBackoff 判定——正常指数退避封顶 5s，
// 远小于它）。
func (p *dialRetryPolicy) afterFailure(prev time.Duration) (wait time.Duration, firstBackoff bool) {
	p.rounds++
	if p.rounds < dialRoundFailureLimit {
		return escalateBackoff(prev), false
	}
	first := !p.announced
	p.announced = true
	return retryLongBackoff, first
}

// backing 报告当前是否处于长退避期（连续失败已达阈值）。拨号循环据此静默
// 逐端口过程日志（QUIC 拨号/边缘不可达/出口探测失败）：退避期的低频探测轮
// 不需要这些重复信息——那正是 journal 被刷爆的来源，最终汇总错误仍由
// dial 的返回值带给调用方。
func (p *dialRetryPolicy) backing() bool {
	return p.rounds >= dialRoundFailureLimit
}

// reset 在一轮拨号成功后调用：清零连续失败计数与播报标记。仅重连航班需要
// （policy 挂在 MasqueClient 上跨航班持久，重建成功即视为故障期结束，下次
// 拆线重新从快速节奏开始并重新播报进入退避）；装配循环成功即 return，实例
// 随之丢弃，无需复位。
func (p *dialRetryPolicy) reset() {
	p.rounds = 0
	p.announced = false
}

// escalateBackoff 是原有的指数退避：0 → 100ms 起，逐轮翻倍至 5s 封顶。
// 0 的语义是「拆线后首次重试立即发起」（runReconnect），装配循环传入的
// prev 也从 0 起步，两个循环共用同一节奏。
func escalateBackoff(prev time.Duration) time.Duration {
	switch {
	case prev == 0:
		return reconnectRetryInitial
	case prev < reconnectRetryMax:
		prev *= 2
		if prev > reconnectRetryMax {
			return reconnectRetryMax
		}
	}
	return prev
}
