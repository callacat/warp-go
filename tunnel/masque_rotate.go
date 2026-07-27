package tunnel

import (
	"context"
	"fmt"
	"log"
	"net"
)

// 本文件实现端点轮询（rotate）特性。atom 顺序展开：先纯函数 nextIdx（无依赖），
// 待 MasqueClient 加池字段（pool/rotateIdx/rotateSize/dialFn/perSlotReconnect）后再上
// pickBundle / reconnectSlot / closePool，最后 openRequestStream 接入。各方法分批写入
// 以保持每步编译可独立验证（详见轮询特性实现计划文档）。
//
// 设计要点（与一期 socks5 的衔接）：
//   - 建池一次付出 N 次 socks5 控制连接（已知泄漏·一期接受），稳态零新增拨号——
//     与"每请求重拨"的无界泄漏相反，维持一期"有界可接受"的前提。
//   - rotateSize==0（直连 / socks5 单连接 / 旧调用方）时所有池代码不执行，pickBundle
//     退化为 currentConnection、reconnectSlot 不被调、Close 走 c.cur——零代价回退。
//   - MasqueClient 的 cur / connMu / reconnectMu 保留供退化路径，不动生产单连接行为。

// nextIdx 是 round-robin 把游标值映射到槽位下标的纯函数：直接对 n 取模，不做 +1。
// 「推进一位」的动作归 pickBundle 的 rotateIdx.Add(1)——atomic.Add 是有状态推进，
// 取模选槽是无状态映射，两者职责分离后纯函数更纯，且消除「cur+1」与「Add(1)」语义
// 重叠造成的上界分析歧义。
//
// n<=0 时返回 0（防御：池未启用或非法时不让调用方拿到负下标越界）。N=1 单元素池恒返 0。
// 用 uint64 算术执行 cur%n：cur 接近 2^64 上界时 wrap 的 Add(1) 已在外层 atomic 完成，
// 这里拿到的 cur 一定是合法 uint64（含 0），取模行为连续——不必先转 int（int 在
// cur 接近 2^63 时溢出有符号）。
func nextIdx(cur uint64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(cur % uint64(n))
}

// dialForRotate 是建池与槽位重连共享的拨号入口：dialFn != nil 走注入实现（测试用），
// 否则走生产 dialAddr（单地址固定端点拨号）。两者签名一致，调用方无需感知差异。
//
// 为何不直接在 buildPool/reconnectSlot 里调 c.dialAddr 而要绕一道：测试要注入假拨号
// 覆盖建池与重连两条路径，但又不能改 dialAddr 的生产签名（它被现有单连接路径共用）。
// 抽一个 rotate 专用入口，nil-check 在此一处，buildPool 与 reconnectSlot 都复用它——
// 测试只注入 dialFn 一次即可覆盖两条路径，生产路径零改动。
func (c *MasqueClient) dialForRotate(ctx context.Context, addr string) (*connBundle, error) {
	if c.dialFn != nil {
		return c.dialFn(ctx, addr)
	}
	return c.dialAddr(ctx, addr)
}

// pickBundle 选下一条开 H3 流的 bundle，是 currentConnection 在池模式的等价物。
// 返回 (bundle, slotIdx, err)：
//   - 池模式（rotateSize>0）：rotateIdx.Add(1) 原子推进游标，nextIdx 取模选槽。
//     返回的 slotIdx >= 0 供 openRequestStream 决定走 reconnectSlot 还是 reconnect。
//   - 退化模式（rotateSize==0）：委托 currentConnection，slotIdx 恒 -1，单连接行为
//     byte-for-byte 不变（含 quicConn.Context().Done() 主动探活）。
//
// 池模式不做主动探活：bundle 死活由 openRequestStream 真实 OpenRequestStream 失败判定，
// 失败才 reconnectSlot(idx, bundle)。这是「延迟到使用点判活」——避免每槽绑定 quicConn
// 类型探测（quic.Conn 是 quic-go 具体类型，测试不可注入），同时与单连接 openRequestStream
// 「用了才知道死」哲学一致。steady state 下 pool[idx] 永不为 nil（buildPool 全填、
// reconnectSlot 只替换不置 nil），故 nil bundle 视为 closed 让上层接力。
//
// 轮转顺序：rotateIdx 初值 0，Add(1) 返回新值，故首调 cur=1 → idx=1，序列为
// [1,2,3,0,1,2,3,0...]。uint64 Add 在 2^64 后 wrap，nextIdx 取模连续无 glitch。
func (c *MasqueClient) pickBundle() (*connBundle, int, error) {
	// 退化模式直接委托 currentConnection——保留单连接的主动探活与所有现有行为。
	if c.rotateSize == 0 {
		b, err := c.currentConnection()
		return b, -1, err
	}

	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.closed {
		return nil, -1, net.ErrClosed
	}
	if len(c.pool) == 0 {
		return nil, -1, net.ErrClosed
	}
	idx := nextIdx(c.rotateIdx.Add(1), c.rotateSize)
	b := c.pool[idx]
	if b == nil {
		// steady state 不应出现；建池失败已降级单连接、reconnectSlot 只替换不置 nil。
		// 这里返 closed 让 openRequestStream 走 reconnectSlot(idx, nil) 接力修复。
		return nil, idx, net.ErrClosed
	}
	return b, idx, nil
}

// reconnectSlot 对 pool[idx] 做槽位级重连。与单连接 reconnect 的关键差异：
//   - 持 perSlotReconnect[idx] 而非全局 reconnectMu——槽 A 重连不阻塞槽 B 的 pickBundle
//     （选槽持 connMu 读，重连持 perSlotReconnect[idx] + 短暂 connMu 写），避免重连风暴。
//   - stale 指针比对判定「是否仍需重连」：若 pool[idx] != stale，说明别的 goroutine 已
//     重建该槽，本次新建的 bundle 直接 close 丢弃，不做无谓替换。纯指针比较，不触
//     quic-go 类型——测试可注入空 bundle 占位。
//   - 拨号在锁外执行（dial 是慢操作 ~8s），仅比对与替换在 connMu 写锁内。
//
// 失败语义：dial 失败 → 保持 stale bundle 在 pool[idx]（不置 nil），下次请求再试时
// OpenRequestStream 仍会失败触发再次 reconnectSlot。退避延至二期。
func (c *MasqueClient) reconnectSlot(ctx context.Context, idx int, stale *connBundle) error {
	if idx < 0 || idx >= c.rotateSize {
		return fmt.Errorf("槽位越界：%d（rotateSize=%d）", idx, c.rotateSize)
	}

	slotMu := &c.perSlotReconnect[idx]
	slotMu.Lock()
	defer slotMu.Unlock()

	// 锁外读一次判定：closed / pool 已被 Close 清空 / pool[idx] 已被他人重建 三者任一即
	// 不重连。**必须先判 closed 与 len(c.pool)，再读 c.pool[idx]**——Close 在 connMu.Lock 下
	// 原子置 closed=true 且 pool=nil，二者顺序无保证，故读切片前先 short-circuit：
	// 若 close 已发生，c.pool 是 nil 切片，c.pool[idx] 直接越界 panic。
	c.connMu.RLock()
	if c.closed || idx >= len(c.pool) {
		c.connMu.RUnlock()
		return net.ErrClosed
	}
	cur := c.pool[idx]
	c.connMu.RUnlock()
	if cur != stale {
		// 别的 goroutine 已重建此槽，本次无活干。
		return nil
	}

	// 拨号在锁外——慢操作不阻塞其他槽的 pickBundle 与 connMu 读者。
	bundle, err := c.dialForRotate(ctx, c.edgeAddrs[idx%len(c.edgeAddrs)])
	if err != nil {
		return err
	}

	c.connMu.Lock()
	// 同上：写路径先判 closed 与 len，再读 c.pool[idx]，与 pickBundle / 上文 RLock 段对称防御，
	// 防止 dial 期间 Close 把 pool=nil 后此处越界 panic。
	if c.closed || idx >= len(c.pool) {
		c.connMu.Unlock()
		bundle.close("client closed or pool dropped")
		return net.ErrClosed
	}
	// 写锁下再判定一次：dial 期间 Close 或其他路径可能已动过此槽。仅当仍等于 stale
	// 才替换，否则把新 bundle 直接丢弃。
	if c.pool[idx] != stale {
		c.connMu.Unlock()
		bundle.close("槽位已被重建")
		return nil
	}
	c.pool[idx] = bundle
	c.connMu.Unlock()

	if stale != nil {
		stale.close("replaced")
	}
	// DoH 由首次 dnsQuery 走 openRequestStream→pickBundle 时锚定在某个槽位（不必然 pool[0]）。
	// 此处任意槽重连都 invalidateDoH 是保守策略：可能误关锚在其他槽的仍在用 DoH，触发一次
	// ≤10s 的 DNS 冷启抖动（dohHandshakeTimeout）。更精确的「仅当重连的槽位承载 DoH 才
	// invalidate」需要给 dohConn 加承载 bundle 字段——二期，不在本期范围。
	// 重连后旧名字解析可能指向不同边缘 IP，invalidate 让下次查询冷启取新解析，开销是一次
	// map 清零 + 可能的一次 DNS 抖动，可接受。
	c.invalidateDoH(nil)
	log.Printf("HTTP/3 槽位 %d 已重建（端点 %s）", idx, c.edgeAddrs[idx%len(c.edgeAddrs)])
	return nil
}
