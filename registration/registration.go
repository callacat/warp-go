package registration

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
)

// Registration holds WARP registration state
type Registration struct {
	ID         string `json:"id"`
	Token      string `json:"token"`
	Account    string `json:"account"`
	KeyType    string `json:"key_type"`
	TunnelType string `json:"tunnel_type"`
	// Both keys are base64 PKIX/SEC1 DER. PublicKeyB64 is written for reference
	// only — nothing reads it back, because the public half is derived from the
	// private key when the registration is loaded.
	PrivateKeyB64 string            `json:"private_key,omitempty"`
	PublicKeyB64  string            `json:"public_key,omitempty"`
	PrivateKey    *ecdsa.PrivateKey `json:"-"`
	PublicKey     crypto.PublicKey  `json:"-"`
	ClientCert    tls.Certificate   `json:"-"`

	// Endpoint info from registration API
	EndpointV4    string `json:"endpoint_v4"`     // e.g. "162.159.198.2"
	EndpointV6    string `json:"endpoint_v6"`     // e.g. "2606:4700:103::2"
	EndpointPorts []int  `json:"endpoint_ports"`  // e.g. [443, 500, 1701, 4500, 4443, 8443, 8095]
	PeerPublicKey string `json:"peer_public_key"` // base64 PKIX DER; pins the edge
	AssignedIPv4  string `json:"assigned_ipv4"`   // e.g. "172.16.0.2"
	AssignedIPv6  string `json:"assigned_ipv6"`   // e.g. "2606:4700:110:..."
}

// RegisterRequest is the JSON body sent to the API
type RegisterRequest struct {
	Key         string `json:"key"`
	InstallID   string `json:"install_id"`
	FcmToken    string `json:"fcm_token"`
	Tos         string `json:"tos"`
	Model       string `json:"model"`
	Serial      string `json:"serial_number"`
	OsVersion   string `json:"os_version"`
	KeyType     string `json:"key_type"`
	TunType     string `json:"tunnel_type"`
	Locale      string `json:"locale"`
	WarpEnabled bool   `json:"warp_enabled"`
}

// EnrollRequest is the JSON body for enrolling a MASQUE key
type EnrollRequest struct {
	Key     string `json:"key"`
	KeyType string `json:"key_type"`
	TunType string `json:"tunnel_type"`
	Name    string `json:"name,omitempty"`
}

const apiBase = "https://api.cloudflareclient.com/v0"

// WARP client version strings (from warp-svc reverse engineering)
const (
	clientVersion = "linux-2026.6.880.0"
	userAgent     = "WARP for Linux"
)

func generateKeyPair() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func publicKeyToBase64(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func privateKeyToBase64(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// publicKeyFromDER parses PKIX DER into an ECDSA public key.
func publicKeyFromDER(der []byte) (*ecdsa.PublicKey, error) {
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("解析公钥失败：%w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("公钥类型为 %T，期望 ECDSA", parsed)
	}
	return pub, nil
}

// decodePublicKey reads the form the registration file stores: base64 PKIX DER.
func decodePublicKey(s string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("公钥不是合法的 base64：%w", err)
	}
	return publicKeyFromDER(der)
}

// pemPublicKeyToBase64 converts the PEM the WARP API returns into the base64
// form stored on disk. Parsing here rather than passing the string through means
// a key the API mangled is caught at registration, not at the first connection.
func pemPublicKeyToBase64(s string) (string, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return "", fmt.Errorf("边缘公钥不是合法的 PEM")
	}
	pub, err := publicKeyFromDER(block.Bytes)
	if err != nil {
		return "", err
	}
	return publicKeyToBase64(pub)
}

// decodePrivateKey reads the form the registration file stores: base64 SEC1 DER
// (with PKCS#8 accepted as well, since either is a valid DER encoding).
func decodePrivateKey(s string) (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("私钥不是合法的 base64：%w", err)
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("无法解码私钥")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("密钥不是 ECDSA 类型")
	}
	return key, nil
}

func buildClientCert(regID string, privKey *ecdsa.PrivateKey) (tls.Certificate, error) {
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(now.Unix()),
		Subject:               pkix.Name{CommonName: fmt.Sprintf("%s.masque2.cloudflareclient.com", regID)},
		NotBefore:             now,
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成证书失败：%w", err)
	}
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: privKey}, nil
}

// extractPeerEndpoints 从登记响应的 peer 端点提取 IPv4/IPv6 主机与端口列表：
//   - v4/v6 含端口时剥掉端口，IPv6 再去掉方括号
//   - v4 与 v6 都为空时回退到 Endpoint.Host（API 可能只返回 host 字段；
//     修复前忽略 Host 导致 endpoint_v4 恒为空 → 扫描回退候选变成 ":443"）
func extractPeerEndpoints(v4, v6 string, ports []int, host string) (string, string, []int) {
	out4, out6 := v4, v6
	if out4 != "" {
		if h, _, err := net.SplitHostPort(out4); err == nil {
			out4 = h
		}
	}
	if out6 != "" {
		if h, _, err := net.SplitHostPort(out6); err == nil {
			out6 = h
		} else {
			out6 = strings.Trim(out6, "[]")
		}
	}
	if out4 == "" && out6 == "" && host != "" {
		out4 = host
	}
	return out4, out6, ports
}

// Register performs WARP registration using the two-step flow the API requires:
//  1. POST /v0/reg with a Curve25519 key — the tunnel type WARP was originally
//     built around, and the only one /reg accepts at creation time
//  2. PATCH /v0/reg/{id} to enroll the MASQUE ECDSA P-256 key, switching the
//     device to tunnel_type=masque
//
// The endpoint address, its port list and the endpoint public key only appear in
// the step 2 response, which is why both calls are needed.
func Register() (*Registration, error) {
	log.Println("正在向 WARP API 注册（两步流程）...")

	// Step 1: register a throwaway Curve25519 key to create the device.
	wgPubKey := generateRandomWgPubkey()
	serial := generateRandomSerial()

	reqBody := RegisterRequest{
		Key:         wgPubKey,
		InstallID:   "",
		FcmToken:    "",
		Tos:         time.Now().Format("2006-01-02T15:04:05.000-07:00"),
		Model:       "PC",
		Serial:      serial,
		KeyType:     "curve25519",
		TunType:     "wireguard",
		Locale:      "en_US",
		WarpEnabled: true,
	}

	bodyJSON, _ := json.Marshal(reqBody)
	url := apiBase + "/reg"
	log.Printf("第 1 步：POST %s", url)

	httpReq, err := http.NewRequest("POST", url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("CF-Client-Version", clientVersion)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("API 请求失败：%w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("注册 API 返回 %d：%s", resp.StatusCode, string(respBody))
	}

	// API wraps response in {"result": {...}}
	var regWrapper struct {
		Result struct {
			ID      string `json:"id"`
			Token   string `json:"token"`
			KeyType string `json:"key_type"`
			TunType string `json:"tunnel_type"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &regWrapper); err != nil {
		return nil, fmt.Errorf("解析注册响应失败：%w，响应体=%s", err, string(respBody))
	}
	regResp := regWrapper.Result
	log.Printf("第 1 步完成：id=%s", regResp.ID)

	// Step 2: Generate MASQUE ECDSA P-256 key and enroll it
	log.Println("正在生成 ECDSA P-256 密钥对...")
	privKey, err := generateKeyPair()
	if err != nil {
		return nil, err
	}

	pubKeyDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("序列化公钥失败：%w", err)
	}
	pubKeyBase64 := base64.StdEncoding.EncodeToString(pubKeyDER)

	enrollBody := EnrollRequest{
		Key:     pubKeyBase64,
		KeyType: "secp256r1",
		TunType: "masque",
	}
	enrollJSON, _ := json.Marshal(enrollBody)

	enrollURL := apiBase + "/reg/" + regResp.ID
	log.Printf("第 2 步：PATCH %s（登记 MASQUE 密钥）", enrollURL)

	enrollReq, err := http.NewRequest("PATCH", enrollURL, strings.NewReader(string(enrollJSON)))
	if err != nil {
		return nil, fmt.Errorf("构造登记请求失败：%w", err)
	}
	enrollReq.Header.Set("Content-Type", "application/json")
	enrollReq.Header.Set("User-Agent", userAgent)
	enrollReq.Header.Set("CF-Client-Version", clientVersion)
	enrollReq.Header.Set("Authorization", "Bearer "+regResp.Token)

	enrollResp, err := httpClient.Do(enrollReq)
	if err != nil {
		return nil, fmt.Errorf("登记 API 请求失败：%w", err)
	}
	defer enrollResp.Body.Close()

	enrollRespBody, _ := io.ReadAll(enrollResp.Body)
	if enrollResp.StatusCode != 200 {
		return nil, fmt.Errorf("登记 API 返回 %d：%s", enrollResp.StatusCode, string(enrollRespBody))
	}

	// API wraps response in {"result": {...}}
	var enrollWrapper struct {
		Result struct {
			ID      string `json:"id"`
			Token   string `json:"token"`
			KeyType string `json:"key_type"`
			TunType string `json:"tunnel_type"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
			Config struct {
				Peers []struct {
					PublicKey string `json:"public_key"`
					Endpoint  struct {
						V4    string `json:"v4"`
						V6    string `json:"v6"`
						Host  string `json:"host"`
						Ports []int  `json:"ports"`
					} `json:"endpoint"`
				} `json:"peers"`
				Interface struct {
					Addresses struct {
						V4 string `json:"v4"`
						V6 string `json:"v6"`
					} `json:"addresses"`
				} `json:"interface"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(enrollRespBody, &enrollWrapper); err != nil {
		return nil, fmt.Errorf("解析登记响应失败：%w，响应体=%s", err, string(enrollRespBody))
	}
	enrollResult := enrollWrapper.Result

	// Extract endpoint info
	var endpointV4, endpointV6 string
	var endpointPorts []int
	var peerPubKey string
	var assignedV4, assignedV6 string

	if len(enrollResult.Config.Peers) > 0 {
		peer := enrollResult.Config.Peers[0]
		peerPubKey = peer.PublicKey
		endpointV4, endpointV6, endpointPorts = extractPeerEndpoints(
			peer.Endpoint.V4, peer.Endpoint.V6, peer.Endpoint.Ports, peer.Endpoint.Host)
	}
	assignedV4 = enrollResult.Config.Interface.Addresses.V4
	assignedV6 = enrollResult.Config.Interface.Addresses.V6

	privB64, err := privateKeyToBase64(privKey)
	if err != nil {
		return nil, fmt.Errorf("编码私钥失败：%w", err)
	}
	pubB64, err := publicKeyToBase64(&privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("编码公钥失败：%w", err)
	}
	// Store one canonical form: the API hands the edge key back as PEM.
	if peerPubKey != "" {
		if peerPubKey, err = pemPublicKeyToBase64(peerPubKey); err != nil {
			return nil, fmt.Errorf("边缘公钥无法解析：%w", err)
		}
	}

	clientCert, err := buildClientCert(enrollResult.ID, privKey)
	if err != nil {
		return nil, fmt.Errorf("构建证书失败：%w", err)
	}

	reg := &Registration{
		ID:            enrollResult.ID,
		Token:         regResp.Token,
		Account:       enrollResult.Account.ID,
		KeyType:       enrollResult.KeyType,
		TunnelType:    enrollResult.TunType,
		PrivateKey:    privKey,
		PublicKey:     &privKey.PublicKey,
		PrivateKeyB64: privB64,
		PublicKeyB64:  pubB64,
		ClientCert:    clientCert,

		EndpointV4:    endpointV4,
		EndpointV6:    endpointV6,
		EndpointPorts: endpointPorts,
		PeerPublicKey: peerPubKey,
		AssignedIPv4:  assignedV4,
		AssignedIPv6:  assignedV6,
	}

	log.Printf("✓ 注册成功：id=%s", reg.ID)
	log.Printf("  边缘：%s（端口：%v），IPv6：%s", endpointV4, endpointPorts, endpointV6)
	log.Printf("  分配地址：%s、%s", assignedV4, assignedV6)
	return reg, nil
}

func generateRandomWgPubkey() string {
	var privateKey [32]byte
	if _, err := rand.Read(privateKey[:]); err != nil {
		return ""
	}
	// Clamp private key per WireGuard spec
	privateKey[0] &= 248
	privateKey[31] &= 127
	privateKey[31] |= 64

	publicKey, err := curve25519.X25519(privateKey[:], curve25519.Basepoint)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(publicKey)
}

func generateRandomSerial() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", b)
}

// PeerPublicKeyVerifier builds a tls.Config.VerifyPeerCertificate callback that
// pins the WARP edge to the endpoint public key handed out at registration.
//
// warp-svc performs the same check — see warp-edge/src/masque.rs:214, which logs
// "Server's public key does not match what is expected." after rendering the
// presented key with boring::pkey::PKeyRef::public_key_to_pem and comparing it
// byte-for-byte against the enrolled endpoint key.
//
// Returns (nil, nil) when the state file carries no endpoint key (registrations
// created before this field was persisted); the caller should then warn that
// pinning is off. A malformed key is reported as an error rather than silently
// downgrading to no pinning.
func (r *Registration) PeerPublicKeyVerifier() (func([][]byte, [][]*x509.Certificate) error, error) {
	if r.PeerPublicKey == "" {
		return nil, nil
	}
	expected, err := decodePublicKey(r.PeerPublicKey)
	if err != nil {
		return nil, err
	}

	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("边缘未提供证书")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("解析边缘证书失败：%w", err)
		}
		got, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("边缘证书公钥类型为 %T，期望 ECDSA", cert.PublicKey)
		}
		if !got.Equal(expected) {
			return fmt.Errorf("边缘公钥与注册时登记的密钥不匹配")
		}
		return nil
	}, nil
}

// Delete deletes the registration from the WARP API
func (r *Registration) Delete() error {
	url := apiBase + "/reg/" + r.ID
	log.Printf("正在注销：%s", url)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("构造注销请求失败：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("CF-Client-Version", clientVersion)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("注销 API 请求失败：%w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("注销 API 返回 %d：%s", resp.StatusCode, string(respBody))
	}

	log.Printf("✓ 已注销：id=%s", r.ID)
	return nil
}

// DeleteRegistration deletes registration from API and removes local state file
func DeleteRegistration(stateFile string) error {
	// Deregistering needs only the id and the bearer token, so it deliberately
	// does not go through Load: a file whose key material this build can no
	// longer parse must still be removable from the API. Otherwise the only way
	// out is deleting the file by hand, which strands the registration on
	// Cloudflare's side with no credential left anywhere to remove it.
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return fmt.Errorf("读取注册文件失败：%w", err)
	}
	var ident struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &ident); err != nil {
		return fmt.Errorf("解析注册文件失败：%w", err)
	}
	if ident.ID == "" || ident.Token == "" {
		return fmt.Errorf("注册文件缺少 id 或 token，无法向 API 注销")
	}

	reg := &Registration{ID: ident.ID, Token: ident.Token}
	if err := reg.Delete(); err != nil {
		log.Printf("警告：API 注销失败（可能已被删除）：%v", err)
	}
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除注册文件失败：%w", err)
	}
	log.Printf("✓ 已删除本地注册文件：%s", stateFile)
	return nil
}

// Save writes registration to file
func (r *Registration) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Load reads registration from file
func Load(path string) (*Registration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("解析注册文件失败：%w", err)
	}
	privKey, err := decodePrivateKey(reg.PrivateKeyB64)
	if err != nil {
		return nil, fmt.Errorf("解码私钥失败：%w", err)
	}
	reg.PrivateKey = privKey
	reg.PublicKey = &privKey.PublicKey
	clientCert, err := buildClientCert(reg.ID, privKey)
	if err != nil {
		return nil, fmt.Errorf("构建客户端证书失败：%w", err)
	}
	reg.ClientCert = clientCert
	return &reg, nil
}
