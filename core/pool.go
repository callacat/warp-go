package core

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
)

// poolDialer 把多个隧道拨号器组成连接池：DialTunnel / ResolveDNS 轮询分配，
// 单个拨号器失败时自动尝试下一个。单连接时原样透传（零回归）。
//
// 动机（v0.5.31，真机实测）：单条共享 QUIC 连接被网络按连接限速到 ~1Mbps
// —— 1 条 / 10 条 / 20 条并发下载的总吞吐恒定（0.75 / 0.99 / 0.74 MB/s），
// 被所有并发流均分。浏览器几十条并发流每条只分到 ~15KB/s → 视频/大资源
// 卡死，而 curl 单流独占能到 0.75MB/s。多连接各自达到独立的限速/丢包均衡，
// 若限速是按连接（5 元组）的则总量可叠加；若为全局带宽则总量不变（无回归）。
type poolDialer struct {
	dials []dialer
	next  atomic.Uint64
}

// serviceProbe 是连接池健康优先轮询的可选接口。实现 EnsureServiceable 的
// 成员（*tunnel.MasqueClient）在池中被优先分配给立即可用的成员；不可用
// 成员（已判死 / 正在重建 / 未建立）被跳过并在后台自愈，避免新请求在其
// 重连航班上阻塞（openRequestStream/establishCONNECT 对 dead 成员默认 join
// 航班等待——池把请求轮询均分到各成员时，死成员会把一半流量拖慢到重连
// 完成，真机表现为「解析半天打不开」）。
type serviceProbe interface {
	EnsureServiceable() bool
}

// newPoolDialer 用一批拨号器构造连接池；len(dials)==1 时 DialTunnel 直接
// 透传，不引入任何轮询开销或语义变化。
func newPoolDialer(dials []dialer) *poolDialer {
	return &poolDialer{dials: dials}
}

// DialTunnel 健康优先轮询：跳过当前不可用（正在重连 / 已判死）的成员，把
// 请求交给立即可用的成员；不可用成员由 EnsureServiceable 在后台自愈，恢复
// 后回到轮询。某成员拨号失败仍按顺序尝试其余成员。全部成员都不可用时退回
// 首选成员的正常拨号——其内部 join 重建航班并等待，与单连接重连语义一致，
// 保证总能等回连接。ctx 取消立即停。
func (p *poolDialer) DialTunnel(ctx context.Context, targetAddr string) (net.Conn, error) {
	n := len(p.dials)
	if n == 0 {
		return nil, errors.New("pool: 拨号器为空")
	}
	if n == 1 {
		return p.dials[0].DialTunnel(ctx, targetAddr)
	}
	idx := int(p.next.Add(1)-1) % n
	dialed := false
	var lastErr error
	for i := 0; i < n; i++ {
		d := p.dials[(idx+i)%n]
		if hp, ok := d.(serviceProbe); ok && !hp.EnsureServiceable() {
			continue
		}
		dialed = true
		conn, err := d.DialTunnel(ctx, targetAddr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx != nil && ctx.Err() != nil {
			break
		}
	}
	if !dialed {
		return p.dials[idx%n].DialTunnel(ctx, targetAddr)
	}
	if lastErr == nil && ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, lastErr
}

// ResolveDNS 同 DialTunnel 的健康优先轮询 + 失败换下一条语义。
func (p *poolDialer) ResolveDNS(ctx context.Context, host string) (net.IP, error) {
	n := len(p.dials)
	if n == 0 {
		return nil, errors.New("pool: 拨号器为空")
	}
	if n == 1 {
		return p.dials[0].ResolveDNS(ctx, host)
	}
	idx := int(p.next.Add(1)-1) % n
	resolved := false
	var lastErr error
	for i := 0; i < n; i++ {
		d := p.dials[(idx+i)%n]
		if hp, ok := d.(serviceProbe); ok && !hp.EnsureServiceable() {
			continue
		}
		resolved = true
		ip, err := d.ResolveDNS(ctx, host)
		if err == nil {
			return ip, nil
		}
		lastErr = err
		if ctx != nil && ctx.Err() != nil {
			break
		}
	}
	if !resolved {
		return p.dials[idx%n].ResolveDNS(ctx, host)
	}
	if lastErr == nil && ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, lastErr
}

// Close 关闭池内全部拨号器，聚合所有错误。
func (p *poolDialer) Close() error {
	var errs []error
	for _, d := range p.dials {
		if err := d.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rotateEdges 把边缘候选表按 i 左旋，让连接池里第 i 个连接从不同候选
// 开始拨号——不同连接落在不同边缘/端口（不同 5 元组），才能各自拿到独立的
// 按连接限速额度。i 可超过表长（循环取模）。
func rotateEdges(edges []string, i int) []string {
	if len(edges) == 0 || len(edges) == 1 || i%len(edges) == 0 {
		return edges
	}
	k := i % len(edges)
	out := make([]string, 0, len(edges))
	out = append(out, edges[k:]...)
	out = append(out, edges[:k]...)
	return out
}

// tunnelConnectionsFor 解析连接数：0 或缺省 → 1（保守单连接），≥1 原样。
func tunnelConnectionsFor(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
