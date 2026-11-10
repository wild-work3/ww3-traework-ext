package tw

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// DeviceInfo 设备指纹（AuthCode 交换必需，官方客户端注入）。
type DeviceInfo struct {
	DeviceID        string `json:"DeviceID"`
	MachineID       string `json:"MachineID"`
	PlatformCode    string `json:"PlatformCode"`
	DeviceType      string `json:"DeviceType"`
	DeviceName      string `json:"DeviceName"`
	DeviceModel     string `json:"DeviceModel"`
	ClientVersion   string `json:"ClientVersion"`
	DevicePublicKey string `json:"DevicePublicKey"`
	DeviceBrand     string `json:"DeviceBrand"`
	DeviceCPU       string `json:"DeviceCPU"`
	OSInfo          string `json:"OSInfo"`
	OSVersion       string `json:"OSVersion"`
}

// AuthCodeResult 是 AuthCode 交换的响应。
type AuthCodeResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Host         string // 交换成功的 origin（后续 API 用）
}

// ExchangeAuthCode 用 AuthCode + CodeVerifier + 设备公钥交换 token。
func (c *Client) ExchangeAuthCode(a *Auth, authCode, codeVerifier string) (*AuthCodeResult, error) {
	if strings.TrimSpace(authCode) == "" {
		return nil, fmt.Errorf("no authCode")
	}
	if strings.TrimSpace(codeVerifier) == "" {
		return nil, fmt.Errorf("no codeVerifier")
	}
	pubPEM, err := devicePublicKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	di := DeviceInfo{
		DeviceID:        a.DeviceID,
		MachineID:       a.MachineID,
		PlatformCode:    "SOLO_PC",
		DeviceType:      "PC",
		DeviceName:      deviceDisplayName(),
		DeviceModel:     DeviceBrand,
		ClientVersion:   IdeVersion,
		DevicePublicKey: pubPEM,
		DeviceBrand:     DeviceBrand,
		DeviceCPU:       "",
		OSInfo:          "windows",
		OSVersion:       OSVersion,
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   di,
		"IDEVersion":   IdeVersion,
	}
	raw, _ := json.Marshal(body)

	// authCode 一次性：4xx（除 429/408）= 终态，立即返回首个 origin 的真实响应；
	// 仅 429/408/5xx / 传输层错误继续尝试下一个 origin。
	var lastErr error
	for _, origin := range c.authCodeOrigins(a.ApiHost) {
		req, err := http.NewRequest(http.MethodPost, origin+EpAuthCodeExchange, bytes.NewReader(raw))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s => %w", origin, err)
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			rj := &AuthCodeRejectedError{Status: resp.StatusCode, Origin: origin, Body: string(data)}
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
				return nil, rj
			}
			lastErr = rj
			continue
		}
		res, err := parseAuthCodeResult(data)
		if err != nil {
			lastErr = fmt.Errorf("%s => %w", origin, err)
			continue
		}
		if res.AccessToken == "" {
			lastErr = fmt.Errorf("%s => %s", origin, truncate(string(data), 160))
			continue
		}
		res.Host = origin
		return res, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate origin")
	}
	return nil, fmt.Errorf("AuthCode ExchangeToken failed: %w", lastErr)
}

// AuthCodeRejectedError 表示 AuthCode 交换被上游拒绝。
type AuthCodeRejectedError struct {
	Status int
	Origin string
	Body   string
}

func (e *AuthCodeRejectedError) Error() string {
	return fmt.Sprintf("AuthCode rejected: %s => HTTP %d %s", e.Origin, e.Status, truncate(e.Body, 160))
}

// AsAuthCodeRejected 报告 err 是否为 *AuthCodeRejectedError。
func AsAuthCodeRejected(err error, target **AuthCodeRejectedError) bool {
	return errors.As(err, target)
}

// IsDeviceLimitReached 判断是否设备数上限（20401 / Device limit reached）。
func IsDeviceLimitReached(err error) bool {
	if err == nil {
		return false
	}
	var rj *AuthCodeRejectedError
	if !errors.As(err, &rj) {
		return false
	}
	return rj.Status == 403 && (strings.Contains(strings.ToLower(rj.Body), "20401") ||
		strings.Contains(strings.ToLower(rj.Body), "device limit"))
}

// authCodeOrigins 候选交换 origin：客户端 Ug 主机优先，再回退回调 host / trae.com.cn。
// 用实例字段（可被 OverrideBase 覆盖）而非包常量，保证联调时首个 origin 即 mock。
func (c *Client) authCodeOrigins(callbackHost string) []string {
	out := []string{}
	for _, h := range []string{c.ugBase(), strings.TrimSpace(callbackHost), "https://api.trae.com.cn"} {
		h = strings.TrimRight(h, "/")
		if h != "" && !containsString(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// parseAuthCodeResult 解析交换响应：token 在 Result.Token/AccessToken，refresh 在 Result.RefreshToken。
func parseAuthCodeResult(data []byte) (*AuthCodeResult, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("authcode exchange parse: %w", err)
	}
	res := &AuthCodeResult{}
	for _, container := range []any{v["Result"], v["result"], v["data"], v} {
		m, ok := container.(map[string]any)
		if !ok {
			continue
		}
		res.AccessToken = mapStr(m, "AccessToken", "accessToken", "access_token", "Token", "token")
		if res.AccessToken != "" {
			res.RefreshToken = mapStr(m, "RefreshToken", "refreshToken", "refresh_token")
			res.ExpiresAt = mapInt64(m, "TokenExpireAt", "tokenExpireAt", "ExpiresAt", "expiresAt", "expiredAt")
			break
		}
	}
	if res.AccessToken == "" {
		return nil, fmt.Errorf("response missing token")
	}
	res.ExpiresAt = normalizeExpiresAt(res.ExpiresAt)
	return res, nil
}

func mapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func mapInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

// devicePublicKeyPEM 生成一次性 ECDSA P256 公私钥对，返回公钥 PEM。
func devicePublicKeyPEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// deviceDisplayName 取主机名作设备名。
func deviceDisplayName() string {
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "PC"
}

// GenPKCE 生成 PKCE code_verifier + code_challenge（S256）。
func GenPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}
