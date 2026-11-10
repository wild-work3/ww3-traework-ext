package tw

import "strings"

// Variant 标识渠道的两个变体。
type Variant string

const (
	// VariantWork 是 traework 变体（function solo_work_lite）。
	VariantWork Variant = "traework"
	// VariantCode 是 traecode 变体（function solo_agent）。
	VariantCode Variant = "traecode"
)

// Profile 是变体间唯一不同的那组参数（function 与定价分组）。
type Profile struct {
	Name             string // traework / traecode
	Function         string
	Prefix           string
	PricingFunctions string
	PricingChannel   string
	PricingPrimary   string
}

// ProfileFor 返回变体档案；未知变体回退到 traework。
func ProfileFor(v Variant) Profile {
	if v == VariantCode {
		return Profile{
			Name:             string(VariantCode),
			Function:         FunctionCode,
			Prefix:           string(VariantCode) + "/",
			PricingFunctions: PricingFuncCode,
			PricingChannel:   PricingChCode,
			PricingPrimary:   PricingPriCode,
		}
	}
	return Profile{
		Name:             string(VariantWork),
		Function:         Function,
		Prefix:           string(VariantWork) + "/",
		PricingFunctions: PricingFuncWork,
		PricingChannel:   PricingChannel,
		PricingPrimary:   PricingPriWork,
	}
}

// ResolveVariant 从渠道配置里的 variant / region 字段解析变体。
//
// region 是历史字段名（两变体并非地理区域差异），这里一并接受：
// traecode / code 视为 code 变体，其余回退 work。
func ResolveVariant(variant, region string) Variant {
	for _, s := range []string{variant, region} {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case string(VariantCode), "code", "solo_agent":
			return VariantCode
		case string(VariantWork), "work", "solo_work_lite":
			return VariantWork
		}
	}
	return VariantWork
}
