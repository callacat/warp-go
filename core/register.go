package core

import (
	"errors"
	"io/fs"
	"os"

	"warp/registration"
)

// Registered 报告本机是否已有可用注册信息（reg.json 存在且可读取）。
// 文件存在但损坏时返回错误 —— 调用方应拒绝覆盖（见 Register 的幂等语义）。
func (s *Server) Registered() (bool, error) {
	_, err := registration.Load(s.opts.StateFile)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// Register 执行注册并保存到 StateFile。注册是幂等的：已有可用注册时原样
// 保留（existing=true），而不是替换 —— 替换会让旧注册在 Cloudflare 侧失去
// 本地凭据，再也无法注销。要更换注册请先 Deregister。
func (s *Server) Register() (existing bool, id string, err error) {
	switch existing, err := registration.Load(s.opts.StateFile); {
	case err == nil:
		// 同步缓存，避免 GUI 轮询 Status 时才补读（Register 后立即刷新注册
		// 卡片需要 s.reg 已就绪）。
		s.mu.Lock()
		s.reg = existing
		s.mu.Unlock()
		return true, existing.ID, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, "", err
	}

	regData, err := registration.Register()
	if err != nil {
		return false, "", err
	}
	if err := regData.Save(s.opts.StateFile); err != nil {
		return false, "", err
	}
	s.mu.Lock()
	s.reg = regData
	s.mu.Unlock()
	return false, regData.ID, nil
}

// Deregister 向 API 注销并删除本地注册信息。仅需 id 与 token，不依赖私钥
// 材料（registration.DeleteRegistration 保证）。
func (s *Server) Deregister() error {
	err := registration.DeleteRegistration(s.opts.StateFile)
	// 注销后清空缓存（无论 API 成功与否，本地文件已删除；下次 Status 会
	// 重新从磁盘判断 Registered）。
	s.mu.Lock()
	s.reg = nil
	s.mu.Unlock()
	return err
}

// RegistrationInfo 返回注册信息的无密钥材料视图；reg.json 缺失时返回
// (nil, nil)，损坏时返回错误。
func (s *Server) RegistrationInfo() (*RegistrationInfo, error) {
	reg, err := registration.Load(s.opts.StateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return registrationView(reg), nil
}

// registrationFileExists 报告注册文件是否存在（不校验内容合法性——
// 注册信息损坏时 Status.Registered 仍为 true，Start 时会报错提示重注册）。
func registrationFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
