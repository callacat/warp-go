package scanner

import (
	"fmt"
	"net"
)

// DefaultPerIPLimit 是每个 CIDR 段默认展开的 IP 数量上限。
// 一段 /24 有 254 个可用地址，16 足以覆盖典型扫描抽样，又不至于刷爆候选表。
const DefaultPerIPLimit = 16

// DefaultV4CIDRs 返回 Cloudflare WARP 边缘的默认 IPv4 CIDR 段。
// 这五段来自对 warp-svc 行为与众包扫描结果的归纳，覆盖常见的存活网段。
func DefaultV4CIDRs() []string {
	return []string{
		"162.159.192.0/24",
		"162.159.193.0/24",
		"162.159.195.0/24",
		"162.159.197.0/24",
		"162.159.198.0/24",
	}
}

// DefaultV6CIDRs 返回 Cloudflare WARP 边缘的默认 IPv6 CIDR 段。
func DefaultV6CIDRs() []string {
	return []string{
		"2606:4700:d0::/48",
		"2606:4700:d1::/48",
	}
}

// DefaultPorts 把注册信息中的端口列表规整为可用的端口序列。
// reg 非空时原样返回；为空或 nil 时兜底到 [443]，与 main.go 中对
// regData.EndpointPorts 的兜底逻辑保持一致。
func DefaultPorts(reg []int) []int {
	if len(reg) == 0 {
		return []int{443}
	}
	return reg
}

// BuildCandidates 把 CIDR 段与端口列表笛卡尔积为候选 host:port 序列。
// perIPLimit<=0 表示不限（全展开该段所有可用地址）；正数表示每段最多取前 N 个。
// 端口在入口处去重并保持首次出现顺序，避免同一 IP 被同一端口重复列出。
// 非法 CIDR 段被静默跳过，不 panic —— 一段坏段不应让整个扫描停摆。
func BuildCandidates(cidrs []string, ports []int, perIPLimit int) []string {
	ports = dedupPorts(ports)

	// 先估算容量，避免反复扩容。每段最多 perIPLimit 个 IP（不限时按 254 估）。
	perIP := perIPLimit
	if perIP <= 0 {
		perIP = 254
	}
	capacity := len(cidrs) * perIP * len(ports)
	if capacity < 0 {
		capacity = 0
	}
	out := make([]string, 0, capacity)

	for _, cidr := range cidrs {
		ips := expandCIDR(cidr, perIPLimit)
		for _, ip := range ips {
			for _, p := range ports {
				out = append(out, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", p)))
			}
		}
	}
	return out
}

// expandCIDR 把单段 CIDR 展开为 IP 列表。
// perIPLimit<=0 表示不限（全展开）；正数表示最多取前 N 个。
// 非法 CIDR 返回 nil，由调用方静默跳过。
// IPv4 跳过 .0（网络地址）与 .255（广播地址）；IPv6 跳过全 0 地址。
// perIPLimit 统计的是“计入候选的地址数”，即网络/广播地址不计入配额。
func expandCIDR(cidr string, perIPLimit int) []net.IP {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	// ParseCIDR 对 "162.159.198.0/24" 返回的 ip 即网络地址本身，
	// 这里统一成 ipnet.IP 作为迭代起点。
	cur := make(net.IP, len(ipnet.IP))
	copy(cur, ipnet.IP)

	var out []net.IP
	for {
		// 起点即网络地址（IPv4 的 .0 / IPv6 的 ::），由 isNetworkOrBroadcast
		// 过滤掉、不计入配额；不断 incIP，直到 cur 落在网段之外即停止。
		if !ipnet.Contains(cur) {
			break
		}
		if !isNetworkOrBroadcast(cur, ipnet) {
			out = append(out, dupIP(cur))
			if perIPLimit > 0 && len(out) >= perIPLimit {
				return out
			}
		}
		incIP(cur)
	}
	return out
}

// incIP 将 ip 原地自增 1（大端序最低位 +1，带进位）。
func incIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

// isNetworkOrBroadcast 判断 ip 是否是网络地址或广播地址，应被排除。
// IPv4：网络地址（主机位全 0，即 .0）与广播地址（主机位全 1，即 .255）跳过。
// IPv6：网络地址（主机位全 0，含 :: 这种全零网段的网络地址）跳过 ——
//
//	/48 段的网络地址形如 2606:4700:d0::，其主机位（低 80 位）全 0，
//	会被下面的“主机位全 0 即网络地址”分支命中而跳过，与起始 pop 一致。
func isNetworkOrBroadcast(ip net.IP, ipnet *net.IPNet) bool {
	// 单主机网段（IPv4 /32、IPv6 /128，掩码全 1）：唯一地址就是主机地址本身，
	// 没有网络地址/广播地址之分，不应被过滤。否则用户用 -scan-cidr 指定
	// 形如 1.2.3.4/32 的单主机段时会得到 0 个候选 —— 静默吞掉，难以排查。
	if ones, bits := ipnet.Mask.Size(); ones == bits {
		return false
	}
	// 标准化为 4 字节形式判定 IPv4 的网络/广播；IPv6 走通用网络地址判定。
	v4 := ip.To4()
	if v4 != nil {
		mask := ipnet.Mask
		if len(mask) != 4 {
			return false
		}
		// 网络地址：主机位全 0。
		network := true
		for i := 0; i < 4; i++ {
			if v4[i]&^mask[i] != 0 {
				network = false
				break
			}
		}
		if network {
			return true
		}
		// 广播地址：主机位全 1。
		broadcast := true
		for i := 0; i < 4; i++ {
			if v4[i]|mask[i] != 0xff {
				broadcast = false
				break
			}
		}
		if broadcast {
			return true
		}
		return false
	}

	// IPv6：跳过网络地址（主机位全 0）。起始 pop 即网络地址，会被这里命中。
	ipMasked := ip.Mask(ipnet.Mask)
	if ipMasked.Equal(ip) {
		return true
	}
	return false
}

// dupIP 复制一份 IP，避免追加进切片的 IP 与后续 incIP 的原地修改共享底层数组。
func dupIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}

// dedupPorts 对端口去重，保持首次出现顺序。
func dedupPorts(ports []int) []int {
	if len(ports) <= 1 {
		// nil 或单元素都可直接返回；空切片保留空语义由 DefaultPorts 兜底。
		// BuildCandidates 直接拿到这里的输出，调用方通常先经 DefaultPorts，
		// 但即便未经，空端口就应产生空候选，而非 panic。
		return ports
	}
	seen := make(map[int]struct{}, len(ports))
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
