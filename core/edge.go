package core

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"warp/registration"
)

// BuildTLSConfig 组装连接 WARP 边缘所需的 TLS 配置（与官方 warp-svc 对齐，
// 见 docs/warp-masque-reverse-engineering.md）：客户端证书 + 边缘公钥固定 +
// TLS 1.3 + H3 ALPN + NIST 曲线优先。注册信息没有边缘公钥时返回 nil 校验器
// （公钥固定禁用，以日志提示），与 Server.Start 原内联逻辑逐字一致。
// 由 Server.Start 与 Android 桥（gui/androidbridge.go）共用。
func BuildTLSConfig(regData *registration.Registration) (*tls.Config, error) {
	verifyEdge, err := regData.PeerPublicKeyVerifier()
	if err != nil {
		return nil, fmt.Errorf("边缘公钥固定初始化失败：%w", err)
	}
	tlsConfig := &tls.Config{
		ServerName:            "consumer-masque-proxy.cloudflareclient.com",
		NextProtos:            []string{"h3"},
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{regData.ClientCert},
		VerifyPeerCertificate: verifyEdge,
		// warp-svc 只提供 NIST 曲线，Go 默认先发 X25519 会引来一次额外的
		// HelloRetryRequest 往返。
		CurvePreferences: []tls.CurveID{tls.CurveP256, tls.CurveP384, tls.CurveP521},
	}
	if verifyEdge != nil {
		log.Println("✓ 边缘公钥固定已启用")
	} else {
		log.Println("⚠ 注册信息中没有边缘公钥，公钥固定已禁用（请重新执行 -reg）")
	}
	return tlsConfig, nil
}

// EdgeIPAuto 是 -ip 的自动模式取值：候选同时含 IPv4 与 IPv6 边缘，拨号层
// 逐个实测、失败自动切换（recvu4IV207cHy）。也是无 -ip 时的默认（CLI 旗标
// 默认值与 Options.EdgeIP 缺省填充均用它；Android 桥无旗标、optsEdgeIP 传
// 空串，ResolveEdgeAddrs 内同样归一到 auto）。
const EdgeIPAuto = "auto"

// autoEdgeAddrs 展开 auto 模式的边缘候选表。
//
// 顺序（recvu4IV207cHy 任务书建议序）：
//
//	v4:443 → v6:443 → v4 其余端口 → v6 其余端口
//
// 443 是 QUIC 最标准端口，两个地址族的 443 排最前能最快区分「地址族被限」
// 还是「端口被限」，跨族切换因此最快（CT103 实测：v4 全端口被 QoS 时
// v4:443(2s) → v6:443 即成功，冷启动 4s 到位；若按族分块（v4 全部再 v6），
// 同场景要先烧完 v4 全部端口 ~14s 才到 v6）。运行态的 reconnect 从上次
// 成功候选开始（addrIdx），两序等价，差异只在冷启动。
//
// 端口列表缺 443 时以 API 返回的首个端口充当首测端口，其余按原序尾随；
// 443 在列表中但非首位时提前到首位（两族各测一次，不产生重复候选）。
// 仅注册了单一地址族时退化为该族顺序展开（等价显式 -ip 4/6 的旧行为）。
// 两族端点都缺失时报错——注册信息不完整应显式失败，而非产出垃圾候选。
func autoEdgeAddrs(reg *registration.Registration) ([]string, error) {
	if reg.EndpointV4 == "" && reg.EndpointV6 == "" {
		return nil, errors.New("注册信息中没有边缘地址（IPv4/IPv6 均缺失）。" +
			"请依次执行 -del 与 -reg 重新注册")
	}
	ports := reg.EndpointPorts
	if len(ports) == 0 {
		ports = []int{443}
	}
	first, rest := ports[0], ports[1:]
	for i, p := range ports {
		if p == 443 {
			first, rest = 443, append(append([]int{}, ports[:i]...), ports[i+1:]...)
			break
		}
	}

	var addrs []string
	for _, host := range []string{reg.EndpointV4, reg.EndpointV6} {
		if host != "" {
			addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(first)))
		}
	}
	for _, p := range rest {
		for _, host := range []string{reg.EndpointV4, reg.EndpointV6} {
			if host != "" {
				addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(p)))
			}
		}
	}
	return addrs, nil
}

// ResolveEdgeAddrs 展开边缘候选地址列表：
//   - cfg.EdgeAddr 非空且 optsEdgeIP 为空 → 应用扫描结果（resolveEdge(cfg.EdgeAddr)）
//   - optsEdgeIP 为 "auto" 或空串（无旗标的默认，含 Android 桥双空路径）→
//     auto 模式：候选同时含 IPv4 与 IPv6 边缘（autoEdgeAddrs），拨号层
//     逐个实测、失败自动切换
//   - optsEdgeIP 为 "4"/"6" → 只按注册信息对应地址族展开端口列表
//     （无端口时默认 443），与旧行为一致
//   - 其余 → 视为显式 host:port（走系统解析器 resolveEdge）
//
// cfg.EdgeAddr 与 optsEdgeIP 均空此前回落 IPv4 注册端点（v0.5.9），CT103
// 实测（recvu4IV207cHy）该默认在 IPv4 边缘被运营商 QoS 时把隧道一起带死
// （v6 正常却够不着）——auto 默认让拨号层自动切换地址族，无需人工判断。
// 显式 "4"/"6" 保持「只连指定」语义不变（向后兼容）。
//
// listenAddr 仅用于日志展示（原 Server.Start 用 Options.ListenAddr 旗标值）；
// Android 桥无 mixed 监听，传空串。显式路径的错误消息与 Server.Start 保持逐字一致。
func ResolveEdgeAddrs(cfg *Config, optsEdgeIP, listenAddr string, regData *registration.Registration) ([]string, error) {
	var edgeAddrs []string
	if strings.TrimSpace(cfg.EdgeAddr) != "" && optsEdgeIP == "" {
		// 扫描应用的边缘地址优先（config.json edge_addr）。
		var err error
		if edgeAddrs, err = resolveEdge(cfg.EdgeAddr); err != nil {
			return nil, fmt.Errorf("edge_addr %q 无法解析：%w", cfg.EdgeAddr, err)
		}
		log.Printf("WARP 代理启动中（边缘=已应用扫描结果 %s，mixed=%s）",
			cfg.EdgeAddr, listenAddr)
	} else {
		// 空（无旗标，含 Android 桥双空路径）与显式 "auto" 同义：跨族候选
		// 自动实测、失败自动切换。
		if optsEdgeIP == "" {
			optsEdgeIP = EdgeIPAuto
		}
		switch optsEdgeIP {
		case EdgeIPAuto:
			var err error
			if edgeAddrs, err = autoEdgeAddrs(regData); err != nil {
				return nil, err
			}
			log.Printf("WARP 代理启动中（边缘=auto 自动测试 IPv4/IPv6，候选=%v，mixed=%s）",
				edgeAddrs, listenAddr)
		case "4", "6":
			endpointHost, other := regData.EndpointV4, "6"
			if optsEdgeIP == "6" {
				endpointHost, other = regData.EndpointV6, "4"
			}
			if endpointHost == "" {
				return nil, fmt.Errorf("注册信息中没有 IPv%s 边缘地址。"+
					"可改用 -ip %s，或依次执行 -del 与 -reg 重新注册", optsEdgeIP, other)
			}
			ports := regData.EndpointPorts
			if len(ports) == 0 {
				ports = []int{443}
			}
			for _, p := range ports {
				edgeAddrs = append(edgeAddrs, net.JoinHostPort(endpointHost, strconv.Itoa(p)))
			}
			log.Printf("WARP 代理启动中（边缘=IPv%s %s 端口=%v，mixed=%s）",
				optsEdgeIP, endpointHost, ports, listenAddr)
		default:
			var err error
			if edgeAddrs, err = resolveEdge(optsEdgeIP); err != nil {
				return nil, fmt.Errorf("-ip %q 既不是 4 或 6，也不是可用地址：%w", optsEdgeIP, err)
			}
			log.Printf("WARP 代理启动中（边缘=%s → %v，mixed=%s）", optsEdgeIP, edgeAddrs, listenAddr)
		}
	}
	return edgeAddrs, nil
}
