package tw

import (
	"net/http"
)

// userAgent 是所有上游请求共用的 UA。
func userAgent() string { return "Trae/" + IdeVersion }

// setRaw 直接写入 map 以保留头名大小写（Go 的 Header.Set 会规范化）。
func setRaw(h http.Header, key, val string) {
	if val == "" {
		return
	}
	h[key] = []string{val}
}

// SOLOHeaders 写入 SOLO 对话/取模型所需的完整头族。
//
// Authorization / X-Cloudide-Token / X-Ide-Token 三处都必须是 `Cloud-IDE-JWT <token>`；
// x-device-id 必须保持小写（部分网关按字面匹配）。
func SOLOHeaders(req *http.Request, a *Auth, stream bool) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", userAgent())

	tok := "Cloud-IDE-JWT " + a.JWT()
	h.Set("Authorization", tok)
	h.Set("X-Cloudide-Token", tok)
	h.Set("X-Ide-Token", tok)
	if uid := a.UID; uid != "" {
		h.Set("X-Uid", uid)
	}
	h.Set("X-App-Id", AppID)
	h.Set("X-App-Version", "default")
	h.Set("X-Ide-Version", IdeVersion)
	h.Set("X-Ide-Version-Code", IdeVersionCode)
	h.Set("X-App-Version-Code", IdeVersionCode)
	h.Set("X-Ide-Version-Type", "stable")
	h.Set("X-Device-Type", "windows")
	h.Set("X-OS-Version", OSVersion)
	h.Set("X-Device-Brand", DeviceBrand)
	h.Set("Request-Traffic-Type", "prod")

	a.RLock()
	mid, did := a.MachineID, a.DeviceID
	a.RUnlock()
	if mid != "" {
		h.Set("X-Machine-Id", mid)
	}
	setRaw(h, "x-device-id", did)
}

// UgHeaders 写入签到/额度接口所需的头族（含设备指纹，缺失会触发 9074）。
func UgHeaders(req *http.Request, a *Auth) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", userAgent())

	tok := "Cloud-IDE-JWT " + a.JWT()
	h.Set("Authorization", tok)
	h.Set("X-Cloudide-Token", tok)
	h.Set("X-Ide-Token", tok)
	if uid := a.UID; uid != "" {
		h.Set("X-Uid", uid)
	}
	h.Set("X-App-Id", AppID)
	h.Set("X-App-Version", "default")
	h.Set("X-Ide-Version", IdeVersion)
	h.Set("X-User-Region", "CN")

	a.RLock()
	did := a.DeviceID
	a.RUnlock()
	setRaw(h, "x-device-brand", DeviceBrand)
	setRaw(h, "x-device-type", "windows")
	setRaw(h, "x-os-version", OSVersion)
	setRaw(h, "x-app-version", IdeVersion)
	setRaw(h, "x-device-id", did)
}

// OAuthHeaders 写入令牌兑换/刷新接口所需的头（无签名头）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
}

// WebPricingHeaders 写入 Web 定价接口所需的头。
func WebPricingHeaders(req *http.Request, a *Auth) {
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("User-Agent", userAgent())
	h.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	h.Set("X-Trae-Client-Type", "web")
	h.Set("X-Trae-User-Timezone", "Asia/Shanghai")
	h.Set("X-Preferenced-Language", "zh-cn")
	h.Set("Referer", WorkHost+"/")
}
