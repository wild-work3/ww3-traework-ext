// Package tw 实现 traework / traecode 渠道的上游协议（SOLO）。
//
// 单插件、双变体：traework（function solo_work_lite）与 traecode（function
// solo_agent）共用同一套账号体系与上游端点，仅在 function 与定价分组上不同。
// 变体通过渠道配置 `variant` / `region` 选择（见 region.go）。
package tw

import "time"

// 上游主机。
const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	WorkHost    = "https://work.trae.cn"
)

// 客户端常量（与旧版 constants.go 保持一致）。
const (
	ClientID        = "en1oxy7wnw8j9n"
	AppID           = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion      = "0.1.52"
	IdeVersionCode  = "20260811"
	PluginVersion   = "2.3.73734"
	DeviceBrand     = "20Y5A002XX"
	OSVersion       = "Windows 10 Pro"
	DefaultConfig   = "glm-5.2"
	Function        = "solo_work_lite"
	FunctionCode    = "solo_agent"
	PricingChannel  = "traework"
	PricingChCode   = "traecode"
	PricingFuncWork = "solo_agent_remote,solo_work_remote,solo_design_remote"
	PricingFuncCode = "solo_agent,solo_agent_remote,solo_work_remote,solo_design_remote"
	PricingPriWork  = "solo_work_remote"
	PricingPriCode  = "solo_agent"
)

// 上游端点。
const (
	EpChat             = "/api/agent/v3/llm_utils_chat"
	EpModels           = "/api/ide/v1/get_detail_param"
	EpExchange         = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo         = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus    = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim     = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage         = "/trae/api/v2/pay/web_user_ent_usage"
	EpModelsPricing    = "/api/remote/v1/models"
	EpAuthCodeExchange = "/trae/api/v3/oauth/ExchangeToken"
)

// softRateResetLoc 是签到/额度展示用的固定 UTC+8 时区。
var softRateResetLoc = time.FixedZone("UTC+8", 8*3600)
