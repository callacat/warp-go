package core

import (
	"crypto/tls"
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

// ResolveEdgeAddrs 展开边缘候选地址列表，镜像 Server.Start 原内联逻辑：
//   - cfg.EdgeAddr 非空且 optsEdgeIP 为空 → 应用扫描结果（resolveEdge(cfg.EdgeAddr)）
//   - optsEdgeIP 为 "4"/"6" → 按注册信息对应地址族展开端口列表（无端口时默认 443）
//   - 其余 → 视为显式 host:port（走系统解析器 resolveEdge）
//
// listenAddr 仅用于日志展示（原 Server.Start 用 Options.ListenAddr 旗标值）；
// Android 桥无 mixed 监听，传空串。错误消息与 Server.Start 保持逐字一致。
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
		switch optsEdgeIP {
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
