package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work3/traework-ext/internal/protocol/authflow"
	"wild-work3/traework-ext/internal/protocol/chat"
	"wild-work3/traework-ext/internal/protocol/cli"
	"wild-work3/traework-ext/internal/protocol/errkind"
	"wild-work3/traework-ext/internal/protocol/meta"
	"wild-work3/traework-ext/internal/protocol/plugin"
	"wild-work3/traework-ext/internal/protocol/stat"
	"wild-work3/traework-ext/internal/tw"
)

// PluginID 是本插件的渠道 ID（core 侧据此落盘/路由）。
const PluginID = "traework"

// Log 是插件级日志器，由 main.go 在解析 --data 后注入。
var Log *cli.Logger

// Config 是渠道配置（channel_config）。
type Config struct {
	Variant string `json:"variant"`
	Region  string `json:"region"`
	Proxy   string `json:"proxy"`
	Timeout int    `json:"timeout"`
	BaseURL string `json:"base_url"`
}

// variant 解析配置里的变体（variant 优先，region 作别名）。
func (c Config) variant() tw.Variant {
	return tw.ResolveVariant(firstNonEmpty(c.Variant, c.Region), "")
}

// profile 返回该变体的档案（function/前缀/定价分组）。
func (c Config) profile() tw.Profile { return tw.ProfileFor(c.variant()) }

// isAlias 判断是否是别名渠道（traecode）：账号级操作须跳过，避免重复执行（issue #36）。
func (c Config) isAlias() bool { return c.variant() == tw.VariantCode }

// Daemon 持有一次进程内的渠道状态。
type Daemon struct {
	dataDir string
	login   *tw.LoginManager
	stat    *stat.Store
}

// NewDaemon 构造 Daemon；dataDir 为空时统计关闭。
func NewDaemon(dataDir string) *Daemon {
	return &Daemon{
		dataDir: dataDir,
		login:   tw.NewLoginManager(dataDir),
		stat:    stat.Open(dataDir),
	}
}

// Serve 是进程入口：按 core 指定的模式跑单次或常驻 stdio 协议。
func Serve(ctx context.Context, mode, dataDir string, in io.Reader, out io.Writer) error {
	d := NewDaemon(dataDir)
	r := registry(d)
	if mode == plugin.ModeCall {
		return r.ServeOnce(ctx, in, out)
	}
	return r.ServeStdio(ctx, in, out)
}

// Manifest 声明插件元信息与能力（只注册这里列出的方法）。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID:   PluginID,
		Name: "TraeWork",
		DefaultChannels: []meta.DefaultChannel{
			{ID: "traework", Name: "TraeWork", Config: map[string]any{"variant": "traework"}},
			{ID: "traecode", Name: "TraeCode", Config: map[string]any{"variant": "traecode"}},
		},
		Version: "1.0.0",
		Kind:    "channel",
		Protocol: meta.ProtocolRng{
			Min: 1,
			Max: 1,
		},
		Execution: meta.Execution{
			Control:            plugin.ModeCall,
			Conversation:       plugin.ModeCall,
			MaxWorkers:         4,
			IdleTimeoutSeconds: 0,
		},
		Capabilities: meta.Capabilities{
			Control: []string{
				"models.list",
				"config.schema",
				"config.get",
				"config.set",
				"auth.methods",
				"auth.begin",
				"auth.status",
				"auth.finish",
				"auth.cancel",
				"accounts.list",
				"accounts.resource_detail",
				"accounts.refresh",
				"accounts.checkin",
				"credits.get",
				"stats.get",
				"health.check",
				"ping",
			},
			Conversation: []string{"chat.generate"},
		},
		Icon:        "trae",
		Color:       "#1E90FF",
		Description: "Trae SOLO（traework / traecode 双变体）渠道插件",
	}
}

// registry 把能力方法表挂到注册表上。
func registry(d *Daemon) *plugin.Registry {
	r := plugin.New()
	r.Lifecycle(Manifest())
	r.Handle("models.list", d.modelsList)
	r.Handle("config.schema", d.configSchema)
	r.Handle("config.get", d.configGet)
	r.Handle("config.set", d.configSet)
	r.Handle("auth.methods", d.authMethods)
	r.Handle("auth.begin", d.authBegin)
	r.Handle("auth.status", d.authStatus)
	r.Handle("auth.finish", d.authFinish)
	r.Handle("auth.cancel", d.authCancel)
	r.Handle("accounts.list", d.accountsList)
	r.Handle("accounts.resource_detail", d.resourceDetail)
	r.Handle("accounts.refresh", d.accountRefresh)
	r.Handle("accounts.checkin", d.accountCheckin)
	r.Handle("credits.get", d.creditsGet)
	r.Handle("stats.get", d.statsGet)
	r.HandleStream("chat.generate", d.generate)
	return r
}

func (d *Daemon) configSchema(_ context.Context, _ Raw) (Raw, *Err) {
	return mustJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"variant": map[string]any{
				"type":        "string",
				"title":       "变体",
				"enum":        []string{string(tw.VariantWork), string(tw.VariantCode)},
				"default":     string(tw.VariantWork),
				"description": "traework 使用 function solo_work_lite；traecode 使用 solo_agent。二者共用同一账号体系。",
			},
			"region": map[string]any{
				"type":        "string",
				"title":       "变体（别名）",
				"enum":        []string{string(tw.VariantWork), string(tw.VariantCode)},
				"description": "历史字段名，等价于 variant。",
			},
			"proxy": map[string]any{
				"type":        "string",
				"title":       "代理",
				"description": "如 http://127.0.0.1:7890，留空直连。",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"title":       "超时（秒）",
				"default":     300,
				"description": "单次非流式请求超时；流式请求由空闲看门狗控制。",
			},
			"base_url": map[string]any{
				"type":        "string",
				"title":       "自定义 API 基址",
				"description": "覆盖插件访问的全部上游主机（Agent/Ug/OAuth/Pricing，调试联调用）。",
			},
		},
	}), nil
}

func (d *Daemon) configGet(_ context.Context, p Raw) (Raw, *Err) {
	return mustJSON(map[string]any{"config": parseConfig(p)}), nil
}

func (d *Daemon) configSet(_ context.Context, p Raw) (Raw, *Err) {
	return mustJSON(map[string]any{"ok": true, "config": parseConfig(p)}), nil
}

func (d *Daemon) authMethods(_ context.Context, _ Raw) (Raw, *Err) {
	return mustJSON(authflow.MethodsResult{
		Region: "cn",
		Methods: []authflow.Method{{
			ID:          "loopback",
			Type:        authflow.TypeLoopback,
			Label:       "浏览器登录（本机回调）",
			Description: "打开 https://www.trae.cn 完成登录，回调到本机随机端口；若未自动完成可粘贴回调 URL。",
			Region:      "cn",
			Default:     true,
		}},
		Note: "登录成功后凭据保存在插件私有目录，不会经 IPC 传输。",
	}), nil
}

func (d *Daemon) authBegin(_ context.Context, p Raw) (Raw, *Err) {
	var req authflow.BeginRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	cfg := parseConfig(p)
	v := tw.ResolveVariant(firstNonEmpty(cfg.Variant, cfg.Region), req.Region)
	br, err := d.login.Begin(v, cfg.BaseURL)
	if err != nil {
		return nil, errf(errkind.Auth, errkind.ScopeAccount, "发起登录失败: %v", err)
	}
	return mustJSON(br), nil
}

func (d *Daemon) authStatus(_ context.Context, p Raw) (Raw, *Err) {
	var req authflow.StatusRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	res, err := d.login.Status(req.FlowID)
	if err != nil {
		return mustJSON(authflow.StatusResult{
			State: authflow.StateFailed,
			Hint:  "登录流程不存在或已过期",
			Error: &authflow.Error{Kind: errkind.NotFound, Message: "登录流程不存在或已过期"},
		}), nil
	}
	return mustJSON(res), nil
}

func (d *Daemon) authFinish(_ context.Context, p Raw) (Raw, *Err) {
	var req authflow.FinishRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	raw := firstNonEmpty(req.CallbackURL, req.Values["callback_url"], req.Values["url"])
	res, err := d.login.Finish(req.FlowID, raw)
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "回调地址无效: %v", err)
	}
	return mustJSON(res), nil
}

func (d *Daemon) authCancel(_ context.Context, p Raw) (Raw, *Err) {
	var req authflow.CancelRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	_ = d.login.Cancel(req.FlowID)
	return mustJSON(map[string]any{"ok": true}), nil
}

func (d *Daemon) accountsList(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	out := []meta.AccountInfo{}
	if cfg.isAlias() {
		// 别名渠道不重复列出账号：账号操作由主渠道统一处理（issue #36）。
		return mustJSON(map[string]any{"accounts": out}), nil
	}
	list, err := tw.ListAuths(d.dataDir)
	if err != nil {
		return nil, errf(errkind.AccountFault, errkind.ScopeAccount, "读取账号列表失败: %v", err)
	}
	prefix := cfg.profile().Prefix
	for _, a := range list {
		status := "ok"
		if a.ExpiresAt > 0 && a.ExpiresAt <= time.Now().Unix() {
			status = "expired"
		}
		name := authFileName(a)
		out = append(out, meta.AccountInfo{
			UID:       name,
			Nickname:  orDefault(a.Nickname, a.UID),
			Status:    status,
			LoginKind: string(authflow.TypeLoopback),
			ExpiresAt: accountExpiresAt(a.ExpiresAt),
			ChannelID: PluginID,
			Region:    a.Region(),
			Prefixes:  []string{prefix},
			AuthFile:  name,
		})
	}
	return mustJSON(map[string]any{"accounts": out}), nil
}

func (d *Daemon) resourceDetail(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	acct, c, err := d.account(uidOf(p), cfg)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "%v", err)
	}
	_, items, err := c.UserResourceDetail(acct)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "读取额度失败: %v", err)
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(items), nil
}

func (d *Daemon) creditsGet(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	empty := map[string]any{"balance": nil, "items": []meta.ResourceItem{}, "unit": "credits"}
	if cfg.isAlias() {
		return mustJSON(empty), nil
	}
	acct, c, err := d.account(uidOf(p), cfg)
	if err != nil {
		return mustJSON(empty), nil
	}
	balance, items, err := c.UserResourceDetail(acct)
	if err != nil {
		return mustJSON(empty), nil
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(map[string]any{"balance": balance, "items": items, "unit": "credits"}), nil
}

func (d *Daemon) accountRefresh(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	if cfg.isAlias() {
		return mustJSON(map[string]any{"ok": true, "message": "别名渠道（traecode）不重复执行账号操作，由主渠道统一处理。"}), nil
	}
	acct, c, err := d.account(uidOf(p), cfg)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "%v", err)
	}
	if acct.RefreshTokenValue() != "" {
		if err := c.RefreshToken(acct); err != nil {
			k := errKindOf(err)
			return nil, errf(k, scopeOfKind(k), "刷新凭据失败: %v", err)
		}
		if _, err := tw.SaveAuth(d.dataDir, acct); err != nil {
			return nil, errf(errkind.AccountFault, errkind.ScopeAccount, "写回凭据失败: %v", err)
		}
	}
	msg := "已刷新"
	if balance, _, err := c.UserResourceDetail(acct); err == nil {
		msg = fmt.Sprintf("已刷新，剩余 %d 积分", balance)
	}
	return mustJSON(map[string]any{"ok": true, "message": msg}), nil
}

func (d *Daemon) accountCheckin(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	if cfg.isAlias() {
		return mustJSON(map[string]any{"ok": true, "message": "别名渠道（traecode）不重复执行签到，由主渠道统一处理。"}), nil
	}
	var req struct {
		Targets []string `json:"targets"`
	}
	_ = json.Unmarshal(p, &req)
	targets := req.Targets
	if len(targets) == 0 {
		if name := uidOf(p); name != "" {
			targets = []string{name}
		} else {
			list, _ := tw.ListAuths(d.dataDir)
			for _, a := range list {
				targets = append(targets, authFileName(a))
			}
		}
	}
	if len(targets) == 0 {
		return nil, errf(errkind.Auth, errkind.ScopeAccount, "该渠道尚未登录，请先在面板完成登录")
	}
	msgs := make([]string, 0, len(targets))
	okCount := 0
	for _, name := range targets {
		acct, c, err := d.account(name, cfg)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		label := orDefault(acct.Nickname, name)
		msg, err := c.DailyCheckin(acct)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		okCount++
		msgs = append(msgs, fmt.Sprintf("%s: %s", label, msg))
	}
	if okCount == 0 {
		return nil, errf(errkind.Client, errkind.ScopeAccount, "签到失败: %s", strings.Join(msgs, "; "))
	}
	return mustJSON(map[string]any{"ok": true, "message": strings.Join(msgs, "; ")}), nil
}

func (d *Daemon) modelsList(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	acct, c, err := d.account("", cfg)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "%v", err)
	}
	ms, err := c.FetchModels(acct)
	if err != nil {
		k := errKindOf(err)
		return nil, errf(k, scopeOfKind(k), "拉取模型失败: %v", err)
	}
	prefix := cfg.profile().Prefix
	if ms == nil {
		ms = []meta.ModelInfo{}
	}
	for i := range ms {
		ms[i].ID = prefix + ms[i].ID
	}
	return mustJSON(map[string]any{"models": ms}), nil
}

func (d *Daemon) statsGet(_ context.Context, p Raw) (Raw, *Err) {
	var req struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(p, &req)
	if req.Days <= 0 {
		req.Days = 7
	}
	if d.stat == nil {
		return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
	}
	recs := d.stat.Query(req.Days)
	filtered := make([]stat.Record, 0, len(recs))
	for _, r := range recs {
		if r.Channel == PluginID {
			filtered = append(filtered, r)
		}
	}
	return mustJSON(stat.Summarize(filtered)), nil
}

// account 解析并返回（必要时刷新）指定账号与其客户端。
func (d *Daemon) account(name string, cfg Config) (*tw.Auth, *tw.Client, error) {
	c := newClientFor(cfg)
	var acct *tw.Auth
	if name != "" {
		a, err := tw.LoadAuth(d.dataDir, name)
		if err != nil {
			return nil, nil, &tw.Error{Kind: errkind.Auth, Msg: fmt.Sprintf("加载账号 %s 失败: %v", name, err)}
		}
		acct = a
	} else {
		list, err := tw.ListAuths(d.dataDir)
		if err != nil {
			return nil, nil, err
		}
		if len(list) > 0 {
			acct = list[0]
		}
	}
	if acct == nil {
		return nil, nil, &tw.Error{Kind: errkind.Auth, Msg: "该渠道尚未登录，请先在面板完成登录"}
	}
	if acct.NeedsRefresh(5*time.Minute) && acct.RefreshTokenValue() != "" {
		if err := c.RefreshToken(acct); err != nil {
			return nil, nil, err
		}
		if _, err := tw.SaveAuth(d.dataDir, acct); err != nil {
			return nil, nil, err
		}
	}
	return acct, c, nil
}

// newClientFor 按配置构造客户端（应用代理/超时/base_url 覆盖）。
func newClientFor(cfg Config) *tw.Client {
	c := tw.NewForVariant(cfg.variant())
	if cfg.Proxy != "" {
		if u, err := url.Parse(cfg.Proxy); err == nil && u.Host != "" {
			tr := &http.Transport{
				Proxy:                 http.ProxyURL(u),
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			}
			c.HTTP.Transport = tr
			c.StreamHTTP.Transport = tr
		}
	}
	if cfg.Timeout > 0 {
		c.HTTP.Timeout = time.Duration(cfg.Timeout) * time.Second
	}
	if cfg.BaseURL != "" {
		c.OverrideBase(cfg.BaseURL)
	}
	return c
}

// recordChat 把一次对话结果写入统计流水。
func (d *Daemon) recordChat(acct *tw.Auth, model string, u *chat.Usage, ok bool, kind string, elapsed time.Duration) {
	if d.stat == nil {
		return
	}
	r := statRecord(model, ok, kind)
	if acct != nil {
		r.Account = authFileName(acct)
		r.Region = acct.Region()
	}
	r.TimeMs = int(elapsed / time.Millisecond)
	if u != nil {
		if u.PromptTokens > 0 {
			r.PromptTokens = u.PromptTokens
		}
		if u.CompletionTokens > 0 {
			r.CompletionTokens = u.CompletionTokens
		}
		if u.TotalTokens > 0 {
			r.TotalTokens = u.TotalTokens
		} else if u.PromptTokens > 0 || u.CompletionTokens > 0 {
			r.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
		if u.Cache != nil {
			if u.Cache.Cached > 0 {
				r.CachedTokens = u.Cache.Cached
			}
			r.CachedReported = u.Cache.CachedReported || u.Cache.WriteReported
		}
		r.FirstTokenMs = int(u.FirstTokenMs)
	}
	d.stat.Append(r)
}

// statRecord 构造一条对话流水（token 未上报时记 -1，绝不写 0）。
func statRecord(model string, ok bool, kind string) stat.Record {
	return stat.Record{
		TS:               time.Now().Format(time.RFC3339),
		Kind:             stat.KindChat,
		Channel:          PluginID,
		Model:            model,
		OK:               ok,
		ErrKind:          kind,
		PromptTokens:     -1,
		CompletionTokens: -1,
		TotalTokens:      -1,
	}
}

// scopeOfKind 把错误类别映射到协议作用域。
func scopeOfKind(k errkind.Kind) errkind.Scope {
	switch k {
	case errkind.HardCredit, errkind.SoftRate, errkind.SessionDead, errkind.Auth, errkind.AccountFault:
		return errkind.ScopeAccount
	case errkind.NotFound, errkind.ModelBlocked, errkind.PromptTooLong:
		return errkind.ScopeModel
	case errkind.Server, errkind.WafBlock, errkind.Conn:
		return errkind.ScopeChannel
	default:
		return errkind.ScopeRequest
	}
}

// errKindOf 从任意 error 中提取 errkind.Kind（默认 Client）。
func errKindOf(err error) errkind.Kind {
	if err == nil {
		return ""
	}
	var te *tw.Error
	if errors.As(err, &te) && te.Kind != "" {
		return te.Kind
	}
	var se *tw.SOLOStreamError
	if errors.As(err, &se) {
		return errkind.Kind(se.Kind())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errkind.Cancel
	}
	return errkind.Client
}

// parseConfig 从请求参数里解出渠道配置。
func parseConfig(p Raw) Config {
	var wrapper struct {
		Config Config `json:"config"`
	}
	_ = json.Unmarshal(p, &wrapper)
	cfg := wrapper.Config
	if cfg.Timeout <= 0 {
		cfg.Timeout = 300
	}
	return cfg
}

// mergeChannelConfig 用请求内嵌的 channel_config 覆盖外层配置（会话面不传 params.config）。
// 不合并会导致 variant/base_url 丢失，变体前缀校验与联调基址全部失效。
func mergeChannelConfig(cfg Config, req chat.GenerateRequest) Config {
	if len(req.ChannelConfig) == 0 {
		return cfg
	}
	var in Config
	_ = json.Unmarshal(req.ChannelConfig, &in)
	if in.Variant != "" {
		cfg.Variant = in.Variant
	}
	if in.Region != "" {
		cfg.Region = in.Region
	}
	if in.Proxy != "" {
		cfg.Proxy = in.Proxy
	}
	if in.BaseURL != "" {
		cfg.BaseURL = in.BaseURL
	}
	if in.Timeout > 0 {
		cfg.Timeout = in.Timeout
	}
	return cfg
}

// uidOf 从请求参数里解出账号 uid/文件名。
func uidOf(p Raw) string {
	var wrapper struct {
		UID       string `json:"uid"`
		Account   string `json:"account"`
		AccountID string `json:"account_uid"`
	}
	_ = json.Unmarshal(p, &wrapper)
	return firstNonEmpty(wrapper.UID, wrapper.AccountID, wrapper.Account)
}

// authFileName 返回账号的凭据文件名（去 .json 即 core 侧 account_uid）。
func authFileName(a *tw.Auth) string {
	if a == nil {
		return ""
	}
	if a.FilePath != "" {
		return tw.AuthFileNameOf(a.FilePath)
	}
	return tw.AuthFileID(a.UID)
}

// accountExpiresAt 把 Unix 秒格式化为 RFC3339；0 表示未知。
func accountExpiresAt(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// mustJSON 序列化；失败即 panic（协议层无法表达序列化错误）。
func mustJSON(v any) Raw {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("traework: marshal failed: %v", err))
	}
	return b
}

// orDefault 返回 s，空则返回 def。
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// firstNonEmpty 返回首个非空白字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
