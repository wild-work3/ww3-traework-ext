package tw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wild-work3/traework-ext/internal/protocol/authflow"
	"wild-work3/traework-ext/internal/protocol/errkind"
)

// 登录实现：loopback_callback + PKCE。
//
// 渠道插件无常驻进程（Execution 为 ModeCall），auth.begin 与后续的 auth.status
// 可能落在不同进程，因此 flow 必须落盘：<data>/auths/flows/<flow_id>.json 文件即状态。
// 本地回调监听只在起本进程内有效，跨进程时用 CanPasteCallback 人工粘贴兜底。

// Flow 是一次在途登录流程的持久化状态。
type Flow struct {
	ID           string         `json:"id"`
	State        authflow.State `json:"state"`
	Method       string         `json:"method"`
	Variant      string         `json:"variant"`
	Base         string         `json:"base,omitempty"`
	MachineID    string         `json:"machineId"`
	DeviceID     string         `json:"deviceId"`
	CodeVerifier string         `json:"codeVerifier"`
	CallbackURL  string         `json:"callbackUrl"`
	CallbackPort int            `json:"callbackPort"`
	AuthURL      string         `json:"authUrl"`
	Host         string         `json:"host"`
	AuthCode     string         `json:"authCode,omitempty"`
	AccessToken  string         `json:"accessToken,omitempty"`
	RefreshToken string         `json:"refreshToken,omitempty"`
	ExpiresAt    int64          `json:"expiresAt,omitempty"`
	UID          string         `json:"uid,omitempty"`
	Nickname     string         `json:"nickname,omitempty"`
	EnterpriseID string         `json:"enterpriseId,omitempty"`
	Error        string         `json:"error,omitempty"`
	ErrorKind    string         `json:"errorKind,omitempty"`
	CreatedAt    int64          `json:"createdAt"`
	ExpiresTS    int64          `json:"expiresTs"`
	Exchanged    bool           `json:"exchanged"`
	AuthFile     string         `json:"authFile,omitempty"`
}

// LoginManager 管理在途登录流程（落盘 + 进程内回调监听）。
type LoginManager struct {
	dataDir string
	mu      sync.Mutex
	servers map[string]*http.Server
	flows   map[string]*Flow // 本进程内创建/更新过的快照
}

// NewLoginManager 创建登录流程管理器。
func NewLoginManager(dataDir string) *LoginManager {
	return &LoginManager{
		dataDir: dataDir,
		servers: map[string]*http.Server{},
		flows:   map[string]*Flow{},
	}
}

// flowsDir 返回 <data>/auths/flows。
func (m *LoginManager) flowsDir() string { return filepath.Join(m.dataDir, "auths", "flows") }

// flowPath 返回某流程的状态文件路径。
func (m *LoginManager) flowPath(id string) string {
	return filepath.Join(m.flowsDir(), id+".json")
}

// saveFlow 原子写流程状态（0600）。
func (m *LoginManager) saveFlow(f *Flow) error {
	if err := os.MkdirAll(m.flowsDir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.flowPath(f.ID), b, 0o600)
}

// loadFlow 读流程状态。
func (m *LoginManager) loadFlow(id string) (*Flow, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("traework: empty flow id")
	}
	b, err := os.ReadFile(m.flowPath(id))
	if err != nil {
		return nil, err
	}
	var f Flow
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// removeFlow 删除流程状态文件。
func (m *LoginManager) removeFlow(id string) {
	_ = os.Remove(m.flowPath(id))
	m.mu.Lock()
	if srv := m.servers[id]; srv != nil {
		_ = srv.Close()
		delete(m.servers, id)
	}
	delete(m.flows, id)
	m.mu.Unlock()
}

// Begin 启动一次 loopback 登录流程：生成设备标识与 PKCE、起本地回调监听、写 flow 文件。
func (m *LoginManager) Begin(variant Variant, base string) (*authflow.BeginResult, error) {
	machineID, err := randHex(32)
	if err != nil {
		return nil, err
	}
	deviceID, err := randNumericID()
	if err != nil {
		return nil, err
	}
	verifier, challenge := GenPKCE()
	flowID, err := randHex(16)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("traework: listen callback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)

	now := time.Now()
	f := &Flow{
		ID:           flowID,
		State:        authflow.StateWaitingCallback,
		Method:       "loopback",
		Variant:      string(variant),
		Base:         strings.TrimRight(strings.TrimSpace(base), "/"),
		MachineID:    machineID,
		DeviceID:     deviceID,
		CodeVerifier: verifier,
		CallbackURL:  callbackURL,
		CallbackPort: port,
		CreatedAt:    now.Unix(),
		ExpiresTS:    now.Add(10 * time.Minute).Unix(),
	}
	f.AuthURL = buildAuthURL(f.Base, challenge, callbackURL, machineID, deviceID)

	srv := &http.Server{
		ReadHeaderTimeout: 15 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/authorize" {
				http.NotFound(w, r)
				return
			}
			m.handleCallback(w, r)
		}),
	}
	m.mu.Lock()
	m.servers[flowID] = srv
	m.flows[flowID] = f
	m.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(5 * time.Minute)
		m.mu.Lock()
		if cur := m.servers[flowID]; cur == srv {
			_ = srv.Close()
			delete(m.servers, flowID)
		}
		m.mu.Unlock()
	}()

	if err := m.saveFlow(f); err != nil {
		return nil, err
	}
	return &authflow.BeginResult{
		FlowID:           flowID,
		Type:             authflow.TypeLoopback,
		Method:           "loopback",
		AuthURL:          f.AuthURL,
		CallbackURL:      callbackURL,
		DebugPort:        port,
		State:            string(authflow.StateWaitingCallback),
		Prompt:           "请在浏览器完成登录；若回调页未自动完成，可复制回调 URL 后粘贴提交。",
		ExpiresAt:        time.Unix(f.ExpiresTS, 0).UTC().Format(time.RFC3339),
		CanPasteCallback: true,
		NextPollMS:       1500,
	}, nil
}

// handleCallback 处理本地回调：解析凭据写入 flow，返回成功/失败页。
func (m *LoginManager) handleCallback(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	var id string
	for fid := range m.servers {
		id = fid
		break
	}
	f := m.flows[id]
	m.mu.Unlock()

	creds := parseCallbackValues(r.URL.RawQuery)
	if f == nil || creds.empty() {
		writeCallbackPage(w, false, "回调缺少登录凭据，请回到应用重试或手动粘贴回调 URL。")
		return
	}
	f.Host = firstNonEmptyStr(creds.Host, f.Host)
	f.AuthCode = firstNonEmptyStr(creds.AuthCode, f.AuthCode)
	f.RefreshToken = firstNonEmptyStr(creds.RefreshToken, f.RefreshToken)
	f.AccessToken = firstNonEmptyStr(creds.AccessToken, f.AccessToken)
	f.State = authflow.StateWaitingUser
	_ = m.saveFlow(f)
	writeCallbackPage(w, true, "登录成功，可以关闭此页面返回应用。")
}

// Status 轮询流程：回调到齐后兑换令牌并取用户信息，成功后落盘凭据。
func (m *LoginManager) Status(flowID string) (*authflow.StatusResult, error) {
	f, err := m.loadFlow(flowID)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > f.ExpiresTS && f.State != authflow.StateSuccess {
		f.State = authflow.StateExpired
		f.Error = "登录流程已超时，请重新发起。"
		f.ErrorKind = errkind.Auth
		_ = m.saveFlow(f)
	}
	switch f.State {
	case authflow.StateSuccess:
		return m.successResult(f), nil
	case authflow.StateFailed:
		return m.failedResult(f), nil
	case authflow.StateExpired:
		return m.failedResult(f), nil
	case authflow.StateCancelled:
		return &authflow.StatusResult{State: authflow.StateCancelled, Hint: "已取消", NextPollMS: 0}, nil
	}

	// 回调已到但尚未兑换。
	if !f.Exchanged && (f.AuthCode != "" || f.RefreshToken != "") {
		if err := m.exchange(f); err != nil {
			f.State = authflow.StateFailed
			f.Error = err.Error()
			f.ErrorKind = flowErrorKind(err)
			_ = m.saveFlow(f)
			return m.failedResult(f), nil
		}
		f.State = authflow.StateSuccess
		f.Exchanged = true
		_ = m.saveFlow(f)
		return m.successResult(f), nil
	}

	return &authflow.StatusResult{
		State:      authflow.StateWaitingCallback,
		Hint:       "等待浏览器回调；可复制回调 URL 后粘贴。",
		NextPollMS: 1500,
	}, nil
}

// Finish 提交人工粘贴的回调 URL（须与 begin 的回调端口一致）。
func (m *LoginManager) Finish(flowID, rawCallback string) (*authflow.StatusResult, error) {
	f, err := m.loadFlow(flowID)
	if err != nil {
		return nil, err
	}
	creds, port := parseCallbackWithPorts(rawCallback)
	if creds.empty() {
		return nil, fmt.Errorf("traework: callback URL has no credentials")
	}
	if port != 0 && port != f.CallbackPort {
		return nil, fmt.Errorf("traework: callback port %d does not match flow port %d", port, f.CallbackPort)
	}
	f.Host = firstNonEmptyStr(creds.Host, f.Host)
	f.AuthCode = firstNonEmptyStr(creds.AuthCode, f.AuthCode)
	f.RefreshToken = firstNonEmptyStr(creds.RefreshToken, f.RefreshToken)
	f.AccessToken = firstNonEmptyStr(creds.AccessToken, f.AccessToken)
	f.State = authflow.StateWaitingUser
	if err := m.saveFlow(f); err != nil {
		return nil, err
	}
	return m.Status(flowID)
}

// Cancel 取消并删除流程。
func (m *LoginManager) Cancel(flowID string) error {
	f, err := m.loadFlow(flowID)
	if err == nil {
		f.State = authflow.StateCancelled
		_ = m.saveFlow(f)
	}
	m.removeFlow(flowID)
	return nil
}

// exchange 用回调凭据兑换/刷新令牌并拉取用户信息，最后落盘凭据。
func (m *LoginManager) exchange(f *Flow) error {
	c := New()
	if strings.TrimSpace(f.Base) != "" {
		c = c.OverrideBase(f.Base)
	}
	a := &Auth{Kind: "traework", Domain: "trae.cn", MachineID: f.MachineID, DeviceID: f.DeviceID, ApiHost: f.Host}
	if f.AuthCode != "" {
		res, err := c.ExchangeAuthCode(a, f.AuthCode, f.CodeVerifier)
		if err != nil {
			return err
		}
		if strings.TrimSpace(res.Host) != "" {
			a.ApiHost = res.Host
		}
		a.AccessToken = res.AccessToken
		a.RefreshToken = res.RefreshToken
		a.ExpiresAt = normalizeExpiresAt(res.ExpiresAt)
	} else {
		a.RefreshToken = f.RefreshToken
		a.AccessToken = f.AccessToken
		if err := c.RefreshToken(a); err != nil {
			return err
		}
	}
	if a.AccessToken == "" {
		return fmt.Errorf("traework: no access token after exchange")
	}
	uid, nickname, enterpriseID, err := c.GetUserInfo(a)
	if err != nil {
		return err
	}
	a.UID = uid
	a.Nickname = nickname
	a.EnterpriseID = enterpriseID
	fileID, err := SaveAuth(m.dataDir, a)
	if err != nil {
		return err
	}
	f.UID = uid
	f.Nickname = nickname
	f.EnterpriseID = enterpriseID
	f.AccessToken = a.AccessToken
	f.RefreshToken = a.RefreshToken
	f.ExpiresAt = a.ExpiresAt
	f.AuthFile = fileID
	return nil
}

// accountExpiresAt 把 Unix 秒转成 RFC3339（<=0 返回空串）。
func accountExpiresAt(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// successResult 把已登录流程转成 authflow.StatusResult（仅事实，无 token）。
func (m *LoginManager) successResult(f *Flow) *authflow.StatusResult {
	return &authflow.StatusResult{
		State: authflow.StateSuccess,
		Hint:  "登录成功",
		Account: &authflow.Account{
			UID:       f.UID,
			Nickname:  firstNonEmptyStr(f.Nickname, f.UID),
			ChannelID: f.Variant,
			Region:    "cn",
			ExpiresAt: accountExpiresAt(f.ExpiresAt),
			LoginKind: string(authflow.TypeLoopback),
			AuthFile:  firstNonEmptyStr(f.AuthFile, AuthFileID(f.UID)),
		},
	}
}

// failedResult 把失败流程转成 authflow.StatusResult。
func (m *LoginManager) failedResult(f *Flow) *authflow.StatusResult {
	return &authflow.StatusResult{
		State: f.State,
		Hint:  f.Error,
		Error: &authflow.Error{Kind: firstNonEmptyStr(f.ErrorKind, errkind.Auth), Message: f.Error},
	}
}

// flowErrorKind 把错误映射为 errkind.Kind。
func flowErrorKind(err error) errkind.Kind {
	if err == nil {
		return ""
	}
	var rej *AuthCodeRejectedError
	if AsAuthCodeRejected(err, &rej) {
		if IsDeviceLimitReached(err) {
			return errkind.AccountFault
		}
		return errkind.Auth
	}
	return errkind.Auth
}

// callbackCreds 是从回调中提取到的凭据集合。
type callbackCreds struct {
	AccessToken  string
	RefreshToken string
	AuthCode     string
	Host         string
}

// empty 报告是否没有任何凭据。
func (c callbackCreds) empty() bool {
	return c.AccessToken == "" && c.RefreshToken == "" && c.AuthCode == ""
}

// parseCallbackValues 解析回调 query，容忍 refreshToken / userJwt / authCodeInfo 等多种命名。
func parseCallbackValues(raw string) callbackCreds {
	c, _ := parseCallbackWithPorts(raw)
	return c
}

// parseCallbackWithPorts 解析回调并返回其中的端口（0 表示未知）。
func parseCallbackWithPorts(raw string) (callbackCreds, int) {
	var c callbackCreds
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c, 0
	}
	port := 0
	// 完整 URL 还是裸 query。
	query := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		query = u.RawQuery
		if p := u.Port(); p != "" {
			fmtSscan(p, &port)
		}
	} else if i := strings.IndexAny(raw, "?"); i >= 0 {
		query = raw[i+1:]
	}
	vals, err := url.ParseQuery(strings.TrimPrefix(query, "?"))
	if err != nil {
		return c, port
	}
	c.AuthCode = firstOf(vals, "authCode", "auth_code", "code")
	c.AccessToken = firstOf(vals, "accessToken", "access_token", "token")
	c.RefreshToken = firstOf(vals, "refreshToken", "refresh_token")
	c.Host = firstOf(vals, "host", "apiHost", "api_host")
	if c.RefreshToken == "" {
		if jwt := firstOf(vals, "userJwt", "user_jwt"); jwt != "" {
			var m map[string]any
			if json.Unmarshal([]byte(jwt), &m) == nil {
				c.RefreshToken = firstNonEmptyStr(strAt(m, "RefreshToken", "refreshToken"), c.RefreshToken)
				c.AccessToken = firstNonEmptyStr(strAt(m, "Token", "token", "AccessToken", "accessToken"), c.AccessToken)
			}
		}
	}
	if c.AuthCode == "" {
		if info := firstOf(vals, "authCodeInfo", "auth_code_info"); info != "" {
			var m map[string]any
			if json.Unmarshal([]byte(info), &m) == nil {
				c.AuthCode = firstNonEmptyStr(strAt(m, "AuthCode", "authCode", "code"), c.AuthCode)
			}
		}
	}
	return c, port
}

// strAt 从 map 中取首个非空字符串字段。
func strAt(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// firstOf 返回 vals 中首个非空值。
func firstOf(v url.Values, keys ...string) string {
	for _, k := range keys {
		if s := strings.TrimSpace(v.Get(k)); s != "" {
			return s
		}
	}
	return ""
}

// buildAuthURL 拼装授权 URL（无 state）。base 为空时回退 ConsoleHost。
func buildAuthURL(base, challenge, callbackURL, machineID, deviceID string) string {
	host := strings.TrimRight(strings.TrimSpace(base), "/")
	if host == "" {
		host = ConsoleHost
	}
	uuid := newUUID()
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", PluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", ClientID)
	q.Set("app_id", AppID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", uuid)
	q.Set("auth_callback_url", callbackURL)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", DeviceBrand)
	q.Set("x_device_type", "windows")
	q.Set("x_os_version", OSVersion)
	q.Set("x_env", "")
	q.Set("x_app_version", IdeVersion)
	q.Set("x_app_type", "stable")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("hide_saas_login", "true")
	q.Set("channel_name", "common")
	q.Set("click_id", "TRAE SOLOSetup-stable-"+PluginVersion)
	return host + "/authorization?" + q.Encode()
}

// writeCallbackPage 输出回调结果页（成功自动关闭）。
func writeCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	title := "登录失败"
	body := msg
	auto := ""
	if ok {
		title = "登录成功"
		auto = "<script>setTimeout(function(){window.close()},1500)</script>"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>"+title+
		"</title></head><body style=\"font-family:sans-serif;text-align:center;margin-top:80px\"><h2>"+title+
		"</h2><p>"+body+"</p>"+auto+"</body></html>")
}

// randHex 返回 n 字节的十六进制串。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randNumericID 返回 15 位十进制设备 ID。
func randNumericID() (string, error) {
	var b [15]byte
	buf := make([]byte, 15)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, v := range buf {
		b[i] = byte('0' + v%10)
	}
	if b[0] == '0' {
		b[0] = '1'
	}
	return string(b[:]), nil
}

// newUUID 返回 v4 UUID 字符串。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fmtSscan 解析十进制端口（避免额外依赖）。
func fmtSscan(s string, out *int) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return
		}
	}
	*out = n
}

var _ = context.Background
