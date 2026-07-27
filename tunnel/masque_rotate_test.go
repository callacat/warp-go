package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

// 端点轮询（rotate）特性的注入式单测。本文件覆盖三个层次：
//   - nextIdx：round-robin 游标的纯函数数学（N=1 单元素池、uint64 取模、池大小边界）。
//   - pickBundle / reconnectSlot / Close：池化建池、轮转选槽、槽位级重连、全池关闭——
//     通过 MasqueClient.dialFn 注入字段塞入假实现，无需触外网/无需真实 QUIC 握手。
//   - openRequestStream 在池模式下的死槽位触发 reconnectSlot、重试一次。
//
// seam 选择：与 tunnel/masque_socks5_test.go 同一思路——走 struct 注入字段而非包级可变
// var。MasqueClient 是 struct，方法值/函数值注入比全局可变点更地道，且少了"忘记 t.Cleanup
// 恢复"的全局污染风险。dialFn 默认 nil 走生产 dialAddr，测试构造 struct 时注入即可。
//
// 这套测试不验证真实 QUIC 行为（那是手测的事），只锁住池调度逻辑的不变量：游标推进
// 顺序、槽位隔离、重连无风暴、Close 全收。真实拨号/手测见 plan 验证 §3。

// TestNextIdx 锁住 round-robin 游标映射数学：把游标值 cur 映射到 [0,n) 的槽位下标。
// 推进动作（rotateIdx.Add(1)）归 pickBundle，本函数只做无状态取模映射——任何抖动都
// 直接表现为请求漏接某槽或重复同槽。
//
// 用例按「游标在该位置时落到哪槽」组织：期望值 = cur % n（直接取模，不 +1）。
func TestNextIdx(t *testing.T) {
	cases := []struct {
		name string
		cur  uint64
		n    int
		want int
	}{
		// 基本映射：游标 0/1/2/3 落到 0/1/2/3 槽。
		{"游标0 n=4", 0, 4, 0},
		{"游标1 n=4", 1, 4, 1},
		{"游标2 n=4", 2, 4, 2},
		{"游标3 n=4", 3, 4, 3},
		// 超过一圈后回绕：游标 4 落 0、5 落 1、7 落 3。
		{"游标4 回绕 n=4", 4, 4, 0},
		{"游标7 回绕 n=4", 7, 4, 3},

		// N=1 单元素池：取模 1 恒为 0，无论 cur 多少都锁第 0 槽。这是退化边界——
		// 池只有一条连接时，per-request 轮转实际就是恒用同一条，但数学必须自洽不 panic。
		{"N=1 游标0 恒锁 0", 0, 1, 0},
		{"N=1 任意 cur 恒锁 0", 7, 1, 0},

		// uint64 接近上界：取模行为连续。(2^64-1) % 4 = 3（2^64 mod 4 = 0，故 -1 mod 4 = 3）；
		// (2^64-2) % 4 = 2。这条验证取模用 uint64 算术而非先转 int（否则近 2^63 溢出有符号）。
		{"uint64 上界 n=4", ^uint64(0), 4, 3},     // 2^64-1 → 3
		{"uint64 上界-1 n=4", ^uint64(0) - 1, 4, 2}, // 2^64-2 → 2
		{"uint64 上界-2 n=4", ^uint64(0) - 2, 4, 1}, // 2^64-3 → 1

		// n<=0 防御：池未被启用或非法值时返回 0 槽，不让调用方拿到负下标越界。
		// 生产路径保证 n>=1，但纯函数应自洽——这条锁住"绝不 panic、绝不负值"。
		{"N=0 防御返 0", 5, 0, 0},
		{"N<0 防御返 0", 5, -3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextIdx(tc.cur, tc.n)
			if got != tc.want {
				t.Fatalf("nextIdx(cur=%d, n=%d) = %d, want %d", tc.cur, tc.n, got, tc.want)
			}
			if got < 0 {
				t.Fatalf("nextIdx 不应返回负值，实际 %d", got)
			}
			// 正常 n>0 时结果必在 [0, n)——锁住不越界，免得 pickBundle 切片访问 panic。
			if tc.n > 0 && got >= tc.n {
				t.Fatalf("nextIdx 结果 %d 越界 [0, %d)", got, tc.n)
			}
		})
	}
}

// fakeBundleEOF 返回一个空的 connBundle 占位。close 对全 nil 字段逐个 nil-check 友好，
// 安全可调用——测试用它喂 pool[i]，纯做指针占位，不触 quic-go 任何具体类型（quicConn
// 等留 nil）。这是我们绕开「*quic.Conn 不可注入」的 seam：测试只锁住池调度指针语义，
// 不锁 quic-go 存活探测（那是手测/库行为）。
func fakeBundleEOF() *connBundle { return &connBundle{} }

// TestPickBundleRotation 锁住池模式轮转顺序。另起池字段，rotateSize=4，连续 8 次 pickBundle
// 断言 idx 序列为 [1,2,3,0,1,2,3,0]——Add(1) 返回新值，故首调从 1 起而非 0。两条不变量：
// (a) 顺序严格 round-robin（无乱序、无漏槽）；(b) 每次 pickBundle 不修改 pool 切片（稳态只读）。
//
// 退化路径（rotateSize==0 委托 currentConnection）不在本测内——currentConnection 依赖
// c.cur.quicConn.Context()，nil quicConn 会 panic，而注入真 quic.Conn 成本远超这条契约的
// 价值。退化路由正确性由 (a) 生产手测 + (b) buildPool 失败仍走单连接路径 两层间接覆盖。
func TestPickBundleRotation(t *testing.T) {
	pool := []*connBundle{fakeBundleEOF(), fakeBundleEOF(), fakeBundleEOF(), fakeBundleEOF()}
	c := &MasqueClient{
		edgeAddrs: []string{"a:443", "b:443", "c:443", "d:443"},
		rotateSize: 4,
		pool:       pool,
	}
	// connMu 零值可用（sync.RWMutex 零值未锁）；perSlotReconnect 此测不触。
	// connMu.RLock/RUnlock 与 atomic.Uint64 零值配合，全测试安全。
	wantSeq := []int{1, 2, 3, 0, 1, 2, 3, 0}
	for i, want := range wantSeq {
		b, slot, err := c.pickBundle()
		if err != nil {
			t.Fatalf("第 %d 次 pickBundle 失败：%v", i, err)
		}
		if slot != want {
			t.Fatalf("第 %d 次 pickBundle slot=%d，want %d", i, slot, want)
		}
		if b != pool[want] {
			t.Fatalf("第 %d 次 pickBundle 返回的 bundle 不等于 pool[%d]", i, want)
		}
		// 稳态不变量：pickBundle 不改 pool 切片元素。
		if len(c.pool) != 4 {
			t.Fatalf("pickBundle 不应动 pool 长度，实际 len=%d", len(c.pool))
		}
	}
}

// TestPickBundleClosed 锁住 closed 池返 ErrClosed：client 关闭后 pickBundle 立即报错，
// 不去碰 pool[idx]（否则可能拿到正在被 Close 并发回收的 bundle）。
func TestPickBundleClosed(t *testing.T) {
	c := &MasqueClient{
		edgeAddrs:   []string{"a:443", "b:443"},
		rotateSize:  2,
		pool:        []*connBundle{fakeBundleEOF(), fakeBundleEOF()},
		closed:      true,
	}
	_, _, err := c.pickBundle()
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed 池应返 ErrClosed，实际 %v", err)
	}
}

// TestPickBundleEmptyPool 锁住空池防御：rotateSize>0 但 pool 空（建池失败降级后理论不应
// 出现，但 reconnect/Close 并发竞态可能短暂出现 nil 槽）——pickBundle 返 ErrClosed 而非 panic。
func TestPickBundleEmptyPool(t *testing.T) {
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 2,
		pool:       nil, // 空 pool
	}
	// nextIdx(Add(1)=1, 2)=1 → pool[1] 越界 → len(pool)==0 短路返 ErrClosed。
	_, _, err := c.pickBundle()
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("空池应返 ErrClosed，实际 %v", err)
	}
}

// TestReconnectSlotRebuilds 锁住核心语义：stale 仍等于 pool[idx] 时，reconnectSlot 用
// dialFn 拨新 bundle 替换 pool[idx]，并 close 旧 stale。这是「同槽死 → 仅重连该槽、
// 不波及其他槽」的不变量。
func TestReconnectSlotRebuilds(t *testing.T) {
	old := fakeBundleEOF()
	var dialCallCount int
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443", "b:443"},
		rotateSize: 2,
		pool:       []*connBundle{old, fakeBundleEOF()},
		perSlotReconnect: make([]sync.Mutex, 2),
		dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
			dialCallCount++
			if addr != "a:443" { // idx=0 → edgeAddrs[0]
				t.Errorf("reconnectSlot(idx=0) 应拨 edgeAddrs[0]=a:443，实际 %s", addr)
			}
			return fakeBundleEOF(), nil
		},
	}

	if err := c.reconnectSlot(context.Background(), 0, old); err != nil {
		t.Fatalf("reconnectSlot 应成功：%v", err)
	}
	if dialCallCount != 1 {
		t.Fatalf("应拨号 1 次，实际 %d", dialCallCount)
	}
	if c.pool[0] == old {
		t.Fatalf("pool[0] 应被替换为新 bundle，仍是旧 stale")
	}
	if c.pool[1] == nil {
		t.Fatalf("槽 1 不应被波及（槽位隔离）")
	}
}

// TestReconnectSlotSkipsIfAlreadyRebuilt 锁住防重连：若 pool[idx] 已被其他 goroutine 重建
// （pool[idx] != stale），本次 reconnectSlot 不拨号、丢弃任何已建 bundle、直接返回 nil。
// 这是双重检查锁定的兑现——并发重连风暴时，N 个 goroutine 触发 reconnectSlot 同槽，
// 只有一个真正重连，其余空转返回。
func TestReconnectSlotSkipsIfAlreadyRebuilt(t *testing.T) {
	stale := fakeBundleEOF()
	newBundle := fakeBundleEOF() // 已被他人重建的占位
	var dialCallCount int
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 1,
		pool:       []*connBundle{newBundle}, // pool[0] 已是新 bundle，不等于 stale
		perSlotReconnect: make([]sync.Mutex, 1),
		dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
			dialCallCount++
			return fakeBundleEOF(), nil
		},
	}

	if err := c.reconnectSlot(context.Background(), 0, stale); err != nil {
		t.Fatalf("已重建的槽位应跳过返 nil 错（实际返 err=%v）", err)
	}
	if dialCallCount != 0 {
		t.Fatalf("已重建的槽位不应拨号，实际拨 %d 次", dialCallCount)
	}
	if c.pool[0] != newBundle {
		t.Fatalf("已重建的 slot 不应被覆盖，pool[0] 应仍是他人建的新 bundle")
	}
}

// TestReconnectSlotClosed 锁住关闭语义：c.closed=true 时 reconnectSlot 不拨号、返 ErrClosed。
// 防止「Close 进行中、槽位重连却在并行建新 bundle」的竞态。
func TestReconnectSlotClosed(t *testing.T) {
	var dialCallCount int
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 1,
		pool:       []*connBundle{fakeBundleEOF()},
		perSlotReconnect: make([]sync.Mutex, 1),
		closed:      true,
		dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
			dialCallCount++
			return fakeBundleEOF(), nil
		},
	}
	err := c.reconnectSlot(context.Background(), 0, c.pool[0])
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed 时应返 ErrClosed，实际 %v", err)
	}
	if dialCallCount != 0 {
		t.Fatalf("closed 时不应拨号，实际拨 %d 次", dialCallCount)
	}
}

// TestReconnectSlotAfterClosePoolDropped 锁住 R1 修复的 panic 防御：Close 在 connMu.Lock 下
// 原子置 closed=true 且 pool=nil。reconnectSlot 进入 connMu.RLock 时若按 err=先读 c.pool[idx]
// 再判 closed，会因 pool 是 nil 切片越界 panic。修复后：先判 closed 或 idx>=len(c.pool) 短路，
// 返 ErrClosed 而非 panic。这条是真并发 race 的纯同步复现——用 c.closed=true + c.pool=nil
// 模拟 Close 完成后的瞬间状态。
func TestReconnectSlotAfterClosePoolDropped(t *testing.T) {
	t.Run("闭锁读 + 拨号后写两段都不 panic", func(t *testing.T) {
		var dialCallCount int
		c := &MasqueClient{
			edgeAddrs:  []string{"a:443", "b:443"},
			rotateSize: 2,
			// pool=nil + closed=true 模拟 Close 完成后的状态。
			pool:            nil,
			perSlotReconnect: make([]sync.Mutex, 2),
			closed:          true,
			dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
				dialCallCount++
				return fakeBundleEOF(), nil
			},
		}
		// stale 用 nil——pickBundle 在 pool[idx]==nil 分支返 (nil, idx, ErrClosed)，
		// openRequestStream 调 reconnectBundle(idx, nil) → reconnectSlot(idx, nil)。
		// 不应 panic，应返 ErrClosed；且不应拨号（短路在拨号前）。
		err := c.reconnectSlot(context.Background(), 1, nil)
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Close 后 pool=nil 时应返 ErrClosed（绝不 panic），实际 %v", err)
		}
		if dialCallCount != 0 {
			t.Fatalf("短路应在拨号前返回，实际拨 %d 次", dialCallCount)
		}
	})
}

// TestReconnectSlotOutOfRange 锁住越界防御：idx 越界（含负数、>= rotateSize）立即返错，
// 不触 perSlotReconnect（否则越界访问切片 panic）。
func TestReconnectSlotOutOfRange(t *testing.T) {
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 2,
		pool:       []*connBundle{fakeBundleEOF(), fakeBundleEOF()},
		perSlotReconnect: make([]sync.Mutex, 2),
	}
	for _, idx := range []int{-1, 2, 99} {
		if err := c.reconnectSlot(context.Background(), idx, nil); err == nil {
			t.Fatalf("idx=%d 越界应返错", idx)
		}
	}
}

// TestReconnectSlotDialError 锁住拨号失败语义：dialFn 返错时 reconnectSlot 透传错误，
// 且 pool[idx] 保持 stale 不变（不置 nil，让下次请求再试），stale 不被 close（仍占槽、
// 可能在摇摇欲坠中恢复——一期接受此乐观）。
func TestReconnectSlotDialError(t *testing.T) {
	stale := fakeBundleEOF()
	dialErr := errors.New("dial boom")
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 1,
		pool:       []*connBundle{stale},
		perSlotReconnect: make([]sync.Mutex, 1),
		dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
			return nil, dialErr
		},
	}
	err := c.reconnectSlot(context.Background(), 0, stale)
	if !errors.Is(err, dialErr) {
		t.Fatalf("应透传 dialErr，实际 %v", err)
	}
	if c.pool[0] != stale {
		t.Fatalf("拨号失败时 pool[0] 应保持 stale，实际被替换")
	}
}

// TestClosePool 锁住池模式 Close：全 pool 的 bundle 都被 close（释放 UDP socket + quic conn），
// pool 切片清空成 nil，closed 标志位翻 true。closeOnce 保证重复 Close 幂等。
// 注：因 connBundle.close 绑死在结构体（Go 无方法重写），本测不直接观察 close 调用，
// 改观察 pool 切片清空与 closed 位——这是指针/数据级可观察不变量，不依赖 mock。
func TestClosePool(t *testing.T) {
	pool := []*connBundle{fakeBundleEOF(), fakeBundleEOF(), fakeBundleEOF()}
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443", "b:443", "c:443"},
		rotateSize: 3,
		pool:       pool,
	}
	// closeOnce 零值可用（sync.Once）。connMu 零值可用。invalidateDoH(nil) 在 nil doh
	// 下走空分支（dohMu 零值可用）。
	if err := c.Close(); err != nil {
		t.Fatalf("Close 应无错：%v", err)
	}
	if !c.closed {
		t.Fatalf("Close 后 closed 应为 true")
	}
	if len(c.pool) != 0 {
		t.Fatalf("Close 后 pool 应清空，实际 len=%d", len(c.pool))
	}
	// 重复 Close 幂等（closeOnce 兑现）——不应 panic、不应因 pool 已 nil 而崩。
	if err := c.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等无错：%v", err)
	}
}

// TestCloseDegenerate 锁住退化 Close：rotateSize==0 时 Close 走 c.cur 单 bundle 路径，
// 与原行为一致。这是「零代价回退」在 Close 上的兑现——池代码不执行，cur 仍是关闭对象。
func TestCloseDegenerate(t *testing.T) {
	cur := fakeBundleEOF()
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443"},
		rotateSize: 0,
		cur:        cur,
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 应无错：%v", err)
	}
	if !c.closed {
		t.Fatalf("Close 后 closed 应为 true")
	}
	if c.cur != nil {
		t.Fatalf("Close 后 cur 应置 nil，实际 %v", c.cur)
	}
}

// TestReconnectBundle_RouteToSlot 锁住 reconnectBundle 的路由分发契约：idx>=0 走 reconnectSlot
// （本测验证 dialFn 被调用、且 dial 的地址是 edgeAddrs[idx] 而非 reconnect 那 addrIdx 序试拨号路径）。
//
// 这是 atom #5 暴露的测试缺口：reconnectBundle 是纯路由 helper，不依赖 h3Client（可注入），
// 但 openRequestStream 接入无测（h3Client 不可注入），故本测间接锁住 reconnectBundle 在池模式
// 真路由到 reconnectSlot 的不变量——若有人把 `idx >= 0` 错写成 `idx > 0`，idx=0 会路由到
// reconnect 而非 reconnectSlot，本测 dialForRotate 路径不会被走，dialCallCount==0 暴露缺陷。
//
// 退化路径 idx==-1 走 reconnect（依赖真 quicConn，currentConnection 在 nil quicConn 上 panic），
// 不在此注入测覆盖——退化回退正确性由 buildPool 失败分支 + 真手测覆盖。
func TestReconnectBundle_RouteToSlot(t *testing.T) {
	var dialCallCount int
	var dialAddr string
	c := &MasqueClient{
		edgeAddrs:  []string{"a:443", "b:443"},
		rotateSize: 2,
		pool:       []*connBundle{fakeBundleEOF(), fakeBundleEOF()},
		perSlotReconnect: make([]sync.Mutex, 2),
		dialFn: func(ctx context.Context, addr string) (*connBundle, error) {
			dialCallCount++
			dialAddr = addr
			return fakeBundleEOF(), nil
		},
	}

	// idx=0 路由到 reconnectSlot → dialForRotate 带 edgeAddrs[0]="a:443"
	if err := c.reconnectBundle(context.Background(), 0, c.pool[0]); err != nil {
		t.Fatalf("idx=0 应路由到 reconnectSlot 成功，实际 err=%v", err)
	}
	if dialCallCount != 1 {
		t.Fatalf("idx>=0 应走 reconnectSlot 拨号（dialFn 被调 1 次），实际 %d", dialCallCount)
	}
	if dialAddr != "a:443" {
		t.Fatalf("reconnectSlot(0) 应拨 edgeAddrs[0]='a:443'，实际 %q", dialAddr)
	}

	// idx=1 同例拨 edgeAddrs[1]="b:443"——锁住槽位与端点绑定关系（不是 reconnect 的 addrIdx 序试）。
	dialAddr = ""
	if err := c.reconnectBundle(context.Background(), 1, c.pool[1]); err != nil {
		t.Fatalf("idx=1 应路由到 reconnectSlot 成功，实际 err=%v", err)
	}
	if dialAddr != "b:443" {
		t.Fatalf("reconnectSlot(1) 应拨 edgeAddrs[1]='b:443'，实际 %q", dialAddr)
	}
}
