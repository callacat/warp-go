package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"warp/registration"
)

// newTestRegistrationFile 构造一份含完整注册字段与合法私钥的 reg.json
// （不依赖真实 Cloudflare API，避免 CI 断网/被限流导致测试失败）。
// registration.Load 会解码 PrivateKeyB64 并构建 ClientCert，因此私钥必须是
// 合法 SEC1 DER base64（缺失/非法时 Load 报错，Status 补读视图失败）。
func newTestRegistrationFile(t *testing.T, dir string) *registration.Registration {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试密钥失败：%v", err)
	}
	der, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		t.Fatalf("序列化私钥失败：%v", err)
	}
	privB64 := base64.StdEncoding.EncodeToString(der)

	reg := &registration.Registration{
		ID:            "test-id-1234",
		Token:         "test-token-5678",
		Account:       "test-account",
		KeyType:       "secp256r1",
		TunnelType:    "masque",
		PrivateKeyB64: privB64,
		EndpointV4:    "162.159.198.2",
		EndpointV6:    "2606:4700:103::2",
		EndpointPorts: []int{443, 500, 1701, 4500},
		AssignedIPv4:  "172.16.0.2",
		AssignedIPv6:  "2606:4700:110:8360:2492:bc30:9e2a:c781",
	}
	stateFile := filepath.Join(dir, "reg.json")
	if err := reg.Save(stateFile); err != nil {
		t.Fatalf("构造注册文件失败：%v", err)
	}
	return reg
}

// TestStatusRegistrationFromDisk 回归测试：GUI 打开时 s.reg 为 nil（只在
// Start/Register 后赋值），Status() 必须从磁盘补读注册信息视图并缓存——
// 否则"注册信息"卡片在未启动时显示为空/不全（用户反馈"注册信息不全"）。
func TestStatusRegistrationFromDisk(t *testing.T) {
	dir := t.TempDir()
	reg := newTestRegistrationFile(t, dir)
	stateFile := filepath.Join(dir, "reg.json")

	s := New(Options{StateFile: stateFile, DataDir: dir})
	if s.reg != nil {
		t.Fatal("前置条件失败：新建 Server 不应持有 s.reg")
	}

	st := s.Status()
	if !st.Registered {
		t.Fatal("Status.Registered 应为 true（reg.json 存在）")
	}
	if st.Registration == nil {
		t.Fatal("Status() 应从磁盘补读注册信息，Registration 不应为 nil")
	}
	if st.Registration.ID != reg.ID {
		t.Errorf("Registration.ID 不匹配：got %q want %q", st.Registration.ID, reg.ID)
	}
	if st.Registration.Account != reg.Account {
		t.Errorf("Registration.Account 不匹配：got %q want %q", st.Registration.Account, reg.Account)
	}
	if st.Registration.AssignedIPv4 != reg.AssignedIPv4 {
		t.Errorf("AssignedIPv4 不匹配：got %q want %q", st.Registration.AssignedIPv4, reg.AssignedIPv4)
	}
	if len(st.Registration.EndpointPorts) == 0 {
		t.Error("EndpointPorts 不应为空")
	}
	// 缓存生效：第二次 Status() 不再报错且视图一致。
	st2 := s.Status()
	if st2.Registration == nil || st2.Registration.ID != reg.ID {
		t.Error("第二次 Status() 应复用缓存视图")
	}
}

// TestStatusRegistrationAfterDeregister 回归测试：注销后 s.reg 清空，
// Status() 应回到 Registered=false（文件已删除）。
func TestStatusRegistrationAfterDeregister(t *testing.T) {
	dir := t.TempDir()
	_ = newTestRegistrationFile(t, dir)
	stateFile := filepath.Join(dir, "reg.json")

	s := New(Options{StateFile: stateFile, DataDir: dir})
	if err := s.Deregister(); err != nil {
		// API 注销可能因缺 token/网络失败返回错误，但本地文件应已删除。
		t.Logf("Deregister 返回（本地文件应已删除）：%v", err)
	}
	if _, serr := os.Stat(stateFile); !os.IsNotExist(serr) {
		t.Fatalf("注销后 reg.json 应被删除（err=%v）", serr)
	}
	st := s.Status()
	if st.Registered {
		t.Error("注销后 Status.Registered 应为 false")
	}
	if st.Registration != nil {
		t.Error("注销后 Status.Registration 应为 nil")
	}
}
