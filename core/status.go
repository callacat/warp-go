package core

import (
	"time"

	"warp/registration"
	"warp/route"
)

// Status 是 Server 的可序列化快照，供 GUI 轮询展示。不包含任何指针、
// channel 或可变内部状态 —— 每次调用生成一份拷贝，可安全跨 Wails 绑定
// 边界传给前端。
type Status struct {
	State      string      `json:"state"` // stopped | starting | running | stopping
	ListenAddr string      `json:"listen_addr,omitempty"`
	EdgeAddrs  []string    `json:"edge_addrs,omitempty"`
	RulesCount int         `json:"rules_count"`
	GeoReady   bool        `json:"geo_ready"`
	SysProxyOn bool        `json:"sys_proxy_on"` // 系统代理当前是否指向本程序
	Registered bool        `json:"registered"`   // 本机是否有可用注册信息
	Stats      route.Stats `json:"stats"`
	StartTime  time.Time   `json:"start_time,omitempty"`
	LastError  string      `json:"last_error,omitempty"`

	Registration *RegistrationInfo `json:"registration,omitempty"`
	Config       *Config           `json:"config,omitempty"` // 生效中的配置快照
}

// RegistrationInfo 是 WARP 注册信息的可序列化视图：与
// registration.Registration 同字段，但剔除私钥 / 证书等密钥材料，GUI 可安全
// 展示（id、账号、端点、分配的隧道内地址等）。
type RegistrationInfo struct {
	ID            string `json:"id"`
	Account       string `json:"account"`
	KeyType       string `json:"key_type"`
	TunnelType    string `json:"tunnel_type"`
	EndpointV4    string `json:"endpoint_v4"`
	EndpointV6    string `json:"endpoint_v6"`
	EndpointPorts []int  `json:"endpoint_ports"`
	AssignedIPv4  string `json:"assigned_ipv4"`
	AssignedIPv6  string `json:"assigned_ipv6"`
}

// registrationView 把注册信息降级为无密钥材料的安全视图。
func registrationView(reg *registration.Registration) *RegistrationInfo {
	if reg == nil {
		return nil
	}
	return &RegistrationInfo{
		ID:            reg.ID,
		Account:       reg.Account,
		KeyType:       reg.KeyType,
		TunnelType:    reg.TunnelType,
		EndpointV4:    reg.EndpointV4,
		EndpointV6:    reg.EndpointV6,
		EndpointPorts: reg.EndpointPorts,
		AssignedIPv4:  reg.AssignedIPv4,
		AssignedIPv6:  reg.AssignedIPv6,
	}
}
