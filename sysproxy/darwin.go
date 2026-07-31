//go:build darwin

package sysproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

// set 对每个启用状态的网络服务执行 networksetup：设置并开启 web 与 secure
// （HTTPS）代理，或关闭两者。SOCKS 防火墙代理（-setsocksfirewallproxy）不
// 动——它只影响需要显式 SOCKS 的程序，而 HTTP(S) 代理设置已覆盖绝大多数
// 系统代理消费方。
func set(host, port string, enabled bool) error {
	services, err := networkServices()
	if err != nil {
		return err
	}
	for _, svc := range services {
		for _, kind := range []string{"web", "secure"} {
			if err := setService(svc, kind, host, port, enabled); err != nil {
				return err
			}
		}
	}
	return nil
}

// networkServices 列出所有网络服务。首行是说明文字（"An asterisk (*) denotes
// that a network service is disabled."），被禁用的服务以 "*" 前缀标记——两者
// 都不能当服务名传给 networksetup。
func networkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("networksetup -listallnetworkservices 失败：%w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("networksetup 未列出任何网络服务")
	}
	var services []string
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if i == 0 || line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("没有可用的网络服务")
	}
	return services, nil
}

// setService 设置或清除单个网络服务上的一类代理（web/secure）。
func setService(svc, kind, host, port string, enabled bool) error {
	if enabled {
		// host 与 port 是独立参数，直接传原始字面量（IPv6 不带方括号，
		// networksetup 自行处理）。
		if err := exec.Command("networksetup", "-set"+kind+"proxy", svc, host, port).Run(); err != nil {
			return fmt.Errorf("networksetup -set%[1]sproxy %[2]q %[3]s %[4]s 失败：%[5]v", kind, svc, host, port, err)
		}
	}
	state := "off"
	if enabled {
		state = "on"
	}
	if err := exec.Command("networksetup", "-set"+kind+"proxystate", svc, state).Run(); err != nil {
		return fmt.Errorf("networksetup -set%[1]sproxystate %[2]q %[3]s 失败：%[4]v", kind, svc, state, err)
	}
	return nil
}
