package tw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work3/traework-ext/internal/protocol/meta"
)

// maxJSONBody 是非流式 JSON 响应的读取上限。
const maxJSONBody = 8 << 20

// Client 是 traework/traecode 上游客户端。同一 Client 可服务两个变体，
// 但 function / 定价分组按构造时传入的 Profile 固定。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client

	AgentHost   string
	UgHost      string
	OAuthHost   string
	PricingHost string
	ClientID    string

	CheckinRetryDelay time.Duration
	IdleTimeout       time.Duration

	Function         string
	PricingFunctions string
	PricingChannel   string
	PricingPrimary   string
}

// New 构造 traework 变体（function solo_work_lite）的客户端。
func New() *Client { return newClient(ProfileFor(VariantWork)) }

// NewTraeCode 构造 traecode 变体（function solo_agent）的客户端。
func NewTraeCode() *Client { return newClient(ProfileFor(VariantCode)) }

// NewForVariant 按变体构造客户端。
func NewForVariant(v Variant) *Client { return newClient(ProfileFor(v)) }

func newClient(p Profile) *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:              &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:        &http.Client{Transport: tr},
		AgentHost:         AgentHost,
		UgHost:            UgHost,
		OAuthHost:         OAuthHost,
		PricingHost:       WorkHost,
		ClientID:          ClientID,
		CheckinRetryDelay: 8 * time.Second,
		IdleTimeout:       DefaultIdleTimeout,
		Function:          p.Function,
		PricingFunctions:  p.PricingFunctions,
		PricingChannel:    p.PricingChannel,
		PricingPrimary:    p.PricingPrimary,
	}
}

func (c *Client) agentBase() string {
	if c.AgentHost != "" {
		return strings.TrimRight(c.AgentHost, "/")
	}
	return AgentHost
}

func (c *Client) ugBase() string {
	if c.UgHost != "" {
		return strings.TrimRight(c.UgHost, "/")
	}
	return UgHost
}

func (c *Client) oauthBase() string {
	if c.OAuthHost != "" {
		return strings.TrimRight(c.OAuthHost, "/")
	}
	return OAuthHost
}

func (c *Client) pricingBase() string {
	if c.PricingHost != "" {
		return strings.TrimRight(c.PricingHost, "/")
	}
	return WorkHost
}

// OverrideBase 把所有上游主机指向同一 base（联调 / 自建代理用）。
// 空 base 不改变任何主机。
func (c *Client) OverrideBase(base string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return c
	}
	c.AgentHost = base
	c.UgHost = base
	c.OAuthHost = base
	c.PricingHost = base
	return c
}

// apiBase 返回某个 Auth 记录的上游 API 主机（用于 AuthCode 交换后的 OAuth 族接口）。
func apiBase(a *Auth) string {
	if a != nil && a.ApiHost != "" {
		return strings.TrimRight(a.ApiHost, "/")
	}
	return OAuthHost
}

// doJSON 发送 JSON 请求并解出 body；status>=400 时返回 *Error。
func (c *Client) doJSON(ctx context.Context, req *http.Request, out any) error {
	req = req.WithContext(ctx)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxJSONBody {
		return errors.New("traework: response exceeds max JSON body")
	}
	if resp.StatusCode >= 400 {
		return errorFor(resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("traework: decode response: %w", err)
	}
	return nil
}

// normalizeExpiresAt 把可能是毫秒的时间戳归一到 Unix 秒。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// RefreshToken 用 refresh token 换新 access token（会轮换 refresh token）。
func (c *Client) RefreshToken(a *Auth) error {
	if a == nil {
		return errors.New("traework: nil auth")
	}
	a.Lock()
	defer a.Unlock()

	if strings.TrimSpace(a.RefreshToken) == "" {
		return errors.New("traework: no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	} else {
		host = strings.TrimRight(host, "/")
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(body))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return errorFor(resp.StatusCode, string(raw))
	}
	var out struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("traework: decode refresh response: %w", err)
	}
	if strings.TrimSpace(out.Result.Token) == "" {
		return errors.New("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = out.Result.Token
	if strings.TrimSpace(out.Result.RefreshToken) != "" {
		a.RefreshToken = out.Result.RefreshToken
	}
	if out.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(out.Result.TokenExpireAt)
	} else if out.Result.TokenExpireDuration > 0 {
		d := out.Result.TokenExpireDuration
		var dur time.Duration
		if d > 1e9 {
			dur = time.Duration(d) * time.Millisecond
		} else {
			dur = time.Duration(d) * time.Second
		}
		a.ExpiresAt = time.Now().Add(dur).Unix()
	}
	return nil
}

// ChatStream 发起流式对话。status>=400 时返回 (nil, status, body, nil)，
// 调用方必须自行检查 status（错误分类延迟到调用方以保留响应体）。
func (c *Client) ChatStream(a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	prepared := PrepareBody(body, c.Function)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)

	ctx, cancel, guard := withIdle(context.Background(), c.IdleTimeout)
	req = req.WithContext(ctx)
	httpClient := c.StreamHTTP
	if httpClient == nil {
		httpClient = c.HTTP
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		if guard.TimedOut() {
			return nil, 0, nil, errors.New("traework: chat stream idle timeout")
		}
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		cancel()
		return nil, resp.StatusCode, raw, nil
	}
	return &idleReadCloser{rc: resp.Body, guard: guard, stop: cancel}, resp.StatusCode, nil, nil
}

// FetchModels 拉取两池模型目录并合并（primary 池优先，错误致命；secondary 池容错）。
func (c *Client) FetchModels(a *Auth) ([]meta.ModelInfo, error) {
	pools := []string{c.Function, OtherFunction(c.Function)}
	seen := map[string]bool{}
	var out []meta.ModelInfo
	var primaryErr error
	for i, fn := range pools {
		models, err := c.fetchModelsForPool(a, fn)
		if err != nil {
			if i == 0 {
				primaryErr = err
				continue
			}
			continue
		}
		for _, m := range models {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 && primaryErr != nil {
		return nil, primaryErr
	}
	if len(out) == 0 {
		return nil, errors.New("traework: no models returned")
	}
	return out, nil
}

type Pricing struct {
	Input  *float64
	Output *float64
}

func (c *Client) fetchModelsForPool(a *Auth, function string) ([]meta.ModelInfo, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"function":            function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	var resp struct {
		ConfigInfoList []poolEntry `json:"config_info_list"`
	}
	if err := c.doJSON(context.Background(), req, &resp); err != nil {
		return nil, err
	}
	var out []meta.ModelInfo
	for _, e := range resp.ConfigInfoList {
		name := strings.TrimSpace(e.ConfigName)
		if name == "" {
			continue
		}
		if e.DisplayConfig.IsCustomModel || strings.HasPrefix(name, "custom_model_") {
			continue
		}
		RecordModelFunc(name, function)
		mi := meta.ModelInfo{ID: name, Name: name}
		if dn := strings.TrimSpace(e.DisplayConfig.DisplayName); dn != "" {
			mi.Name = dn
		}
		ctxWin, maxTok, images := e.metaOf()
		if ctxWin > 0 {
			v := ctxWin
			mi.Context = &v
		}
		_ = maxTok
		if images {
			mi.Capabilities.Vision = true
		}
		out = append(out, mi)
	}
	return out, nil
}

type poolEntry struct {
	ConfigName    string          `json:"config_name"`
	ContextWindow json.RawMessage `json:"context_window"`
	DisplayConfig struct {
		DisplayName   string `json:"display_name"`
		IsCustomModel bool   `json:"is_custom_model"`
		Multimodal    bool   `json:"multimodal"`
	} `json:"display_config"`
	ModelDetailList json.RawMessage `json:"model_detail_list"`
}

func (e poolEntry) metaOf() (ctxWindow, maxTokens int, images bool) {
	if len(e.ContextWindow) > 0 {
		var cw struct {
			Dev int `json:"dev"`
			Max int `json:"max"`
		}
		if json.Unmarshal(e.ContextWindow, &cw) == nil {
			ctxWindow = cw.Max
			if ctxWindow == 0 {
				ctxWindow = cw.Dev
			}
		}
	}
	if len(e.ModelDetailList) > 0 {
		var dl []struct {
			MaxTokens int `json:"max_tokens"`
		}
		if json.Unmarshal(e.ModelDetailList, &dl) == nil && len(dl) > 0 {
			maxTokens = dl[0].MaxTokens
		}
	}
	return ctxWindow, maxTokens, e.DisplayConfig.Multimodal
}

// FetchModelPricing 拉取定价（credits per 1M tokens 等）。
func (c *Client) FetchModelPricing(a *Auth) (map[string]Pricing, error) {
	url := c.pricingBase() + EpModelsPricing +
		"?functions=" + c.PricingFunctions + "&show_custom_model=true"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	WebPricingHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, errorFor(resp.StatusCode, string(raw))
	}
	var out struct {
		Data struct {
			List []struct {
				Models []struct {
					Name        string          `json:"name"`
					Consumption json.RawMessage `json:"consumption_rate"`
					Discount    json.RawMessage `json:"discount"`
				} `json:"models"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("traework: decode pricing: %w", err)
	}
	pricingByModel := map[string]Pricing{}
	for _, group := range out.Data.List {
		for _, m := range group.Models {
			name := strings.TrimSpace(m.Name)
			if name == "" {
				continue
			}
			p := parseTraeFeatures(m.Consumption, m.Discount)
			if p.Input == nil && p.Output == nil {
				continue
			}
			if _, exists := pricingByModel[name]; !exists {
				pricingByModel[name] = p
			}
		}
	}
	return pricingByModel, nil
}

type featureData struct {
	Enable bool `json:"enable"`
	Data   struct {
		Rate            float64 `json:"rate"`
		ConsumptionRate float64 `json:"consumption_rate"`
	} `json:"data"`
}

func parseTraeFeatures(consumption, discount json.RawMessage) Pricing {
	var p Pricing
	var base float64
	if len(consumption) > 0 {
		var f featureData
		if json.Unmarshal(consumption, &f) == nil && f.Enable {
			base = f.Data.Rate
		}
	}
	rate := base
	if len(discount) > 0 {
		var f featureData
		if json.Unmarshal(discount, &f) == nil && f.Enable && f.Data.ConsumptionRate > 0 {
			rate = f.Data.ConsumptionRate
		}
	}
	if rate > 0 {
		v := rate
		p.Input = &v
		p.Output = &v
	}
	return p
}

// CheckinStatus 查询当日签到状态与可用积分。
func (c *Client) CheckinStatus(a *Auth) (checkedIn bool, credits int64, enable bool, err error) {
	var out struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	req, e := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if e != nil {
		return false, 0, false, e
	}
	UgHeaders(req, a)
	if e := c.doJSON(context.Background(), req, &out); e != nil {
		return false, 0, false, e
	}
	if out.Success != nil && !*out.Success {
		msg := firstNonEmptyStr(out.Message, out.Msg)
		return false, 0, false, fmt.Errorf("checkin status failed: %s", msg)
	}
	return out.CheckedIn, out.Credits, out.Enable, nil
}

// CheckinClaim 执行签到（9074 限流时重试一次）。
func (c *Client) CheckinClaim(a *Auth) error {
	delay := c.CheckinRetryDelay
	if delay <= 0 {
		delay = 8 * time.Second
	}
	for attempt := 0; attempt < 2; attempt++ {
		var out struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Msg     string `json:"msg"`
			Success *bool  `json:"success"`
		}
		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		UgHeaders(req, a)
		if err := c.doJSON(context.Background(), req, &out); err != nil {
			return err
		}
		if out.Code == 9074 && attempt == 0 {
			time.Sleep(delay)
			continue
		}
		if out.Code != 0 {
			return fmt.Errorf("checkin claim code=%d msg=%s", out.Code, firstNonEmptyStr(out.Message, out.Msg))
		}
		if out.Success != nil && !*out.Success {
			return fmt.Errorf("checkin claim failed: %s", firstNonEmptyStr(out.Message, out.Msg))
		}
		return nil
	}
	return errors.New("checkin claim rate limited: code=9074")
}

// DailyCheckin 幂等签到：已签到/未开放时返回提示而非错误。
func (c *Client) DailyCheckin(a *Auth) (string, error) {
	checked, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return "", err
	}
	if checked {
		return "已签到", nil
	}
	if !enable {
		return "", errors.New("checkin disabled")
	}
	if err := c.CheckinClaim(a); err != nil {
		return "", err
	}
	checked, credits, _, err := c.CheckinStatus(a)
	if err != nil {
		return "", err
	}
	if !checked {
		return "", errors.New("checkin verification failed: checked_in=false")
	}
	return fmt.Sprintf("签到成功，当前积分 %d", credits), nil
}

type entPackage struct {
	EntitlementBaseInfo struct {
		AvailableEndpoint int `json:"available_endpoint"`
		ProductID         int `json:"product_id"`
		Quota             struct {
			CreditsLimit float64 `json:"credits_limit"`
		} `json:"quota"`
		EntitlementID string `json:"entitlement_id"`
		PackageName   string `json:"package_name"`
		PackageType   string `json:"package_type"`
	} `json:"entitlement_base_info"`
	DisplayDesc string `json:"display_desc"`
	GroupName   string `json:"group_name"`
	GroupType   int    `json:"group_type"`
	ExpireTime  int64  `json:"expire_time"`
	Usage       struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
}

func (p entPackage) usable() bool { return p.EntitlementBaseInfo.AvailableEndpoint != 1 }

func (p entPackage) remain() int64 {
	limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
	used := int64(p.Usage.CreditsAmount)
	if limit-used < 0 {
		return 0
	}
	return limit - used
}

// fetchEntUsage 拉取权益包用量，返回包列表与总剩余额度。
func (c *Client) fetchEntUsage(a *Auth) ([]entPackage, int64, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	if err != nil {
		return nil, 0, err
	}
	UgHeaders(req, a)
	var out struct {
		UserEntitlementPackList []entPackage `json:"user_entitlement_pack_list"`
	}
	if err := c.doJSON(context.Background(), req, &out); err != nil {
		return nil, 0, err
	}
	var remain int64
	for _, p := range out.UserEntitlementPackList {
		if p.usable() {
			remain += p.remain()
		}
	}
	return out.UserEntitlementPackList, remain, nil
}

// UserResourceDetail 返回可展示的资源项与总剩余额度。
func (c *Client) UserResourceDetail(a *Auth) (int64, []meta.ResourceItem, error) {
	packs, remain, err := c.fetchEntUsage(a)
	if err != nil {
		return 0, nil, err
	}
	var items []meta.ResourceItem
	for _, p := range packs {
		limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
		if limit <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		rem := p.remain()
		total := float64(limit)
		usedF := float64(used)
		remainF := float64(rem)
		name := firstNonEmptyStr(p.GroupName, p.DisplayDesc, p.EntitlementBaseInfo.PackageName, p.EntitlementBaseInfo.PackageType)
		if name == "" {
			name = fmt.Sprintf("套餐 (group_type=%d)", p.GroupType)
		}
		items = append(items, meta.ResourceItem{
			Name:     name,
			Total:    &total,
			Used:     &usedF,
			Remain:   &remainF,
			Unit:     "credits",
			Usable:   p.usable(),
			Key:      p.EntitlementBaseInfo.EntitlementID,
			ExpireAt: unixDate(p.ExpireTime),
		})
	}
	return remain, items, nil
}

// GetUserInfo 拉取用户信息（UID / 昵称 / 企业 ID）。
func (c *Client) GetUserInfo(a *Auth) (uid, nickname, enterpriseID string, err error) {
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion})
	req, e := http.NewRequest(http.MethodPost, apiBase(a)+EpUserInfo, bytes.NewReader(body))
	if e != nil {
		return "", "", "", e
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	var out struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if e := c.doJSON(context.Background(), req, &out); e != nil {
		return "", "", "", e
	}
	return out.Result.UserID, out.Result.ScreenName, out.Result.EnterpriseID, nil
}

// unixDate 把 Unix 秒格式化为固定 UTC+8 的 RFC3339；<=0 返回空串。
func unixDate(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(softRateResetLoc).Format(time.RFC3339)
}

// firstNonEmptyStr 返回首个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstInt64 返回首个 >0 的整数（用于 ExpiresAt 兜底）。
func firstInt64(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// parseRate 把可能带 "credits" 单位的速率字符串解析为浮点。
func parseRate(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
