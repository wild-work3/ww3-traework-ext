// Command traework-ext 是 TraeWork 渠道插件（traework / traecode 两变体）。
//
// 架构（照 workbuddy-ext / qoder-ext）：无常驻进程，core 每次以 `call` 起一个子进程。
// CLI 只做「本机调试外壳」——每个命令都通过 spawnInvoke 起自己的 `call` 子进程走真实
// IPC，没有 CLI 专用捷径（否则调试路径与生产路径分叉）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"wild-work3/traework-ext/internal/channel"
	"wild-work3/traework-ext/internal/protocol/authflow"
	"wild-work3/traework-ext/internal/protocol/chat"
	"wild-work3/traework-ext/internal/protocol/cli"
	"wild-work3/traework-ext/internal/protocol/plugin"
	"wild-work3/traework-ext/internal/tw"
)

const pluginName = "traework-ext"

// dataDirGlobal 是解析后的数据目录，供 spawnInvoke 传给子进程。
var dataDirGlobal string

// netFlags 是所有联网命令共享的开关。
type netFlags struct {
	data    *string
	config  *string
	region  *string
	auth    *string
	proxy   *string
	baseURL *string
	timeout *int
}

// addNetFlags 在命令上注册联网开关。
func addNetFlags(c *cli.Command) netFlags {
	return netFlags{
		data:    c.Str("data", "", "插件数据目录（默认取 $WW3_DATA 或可执行文件所在目录）"),
		config:  c.Str("config", "", "渠道配置 JSON（可写 @file 引用文件）"),
		region:  c.Str("region", os.Getenv("WW3_REGION"), "变体：traework | traecode"),
		auth:    c.Str("auth", os.Getenv("WW3_AUTH"), "账号 uid（凭据文件名去 .json）"),
		proxy:   c.Str("proxy", os.Getenv("WW3_PROXY"), "HTTP 代理"),
		baseURL: c.Str("base-url", os.Getenv("WW3_BASE_URL"), "覆盖 Agent 主机（调试用）"),
		timeout: c.Int("timeout", 300, "HTTP 超时（秒）"),
	}
}

// cfg 组装渠道配置 map（--config 为底，命令行覆盖）。
func (n netFlags) cfg() map[string]any {
	m := map[string]any{}
	if raw := strings.TrimSpace(cli.ConfigArg(*n.config)); raw != "" {
		var wrapper struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapper); err == nil && wrapper.Config != nil {
			m = wrapper.Config
		} else {
			var flat map[string]any
			if json.Unmarshal([]byte(raw), &flat) == nil {
				m = flat
			}
		}
	}
	if v := strings.TrimSpace(*n.region); v != "" {
		m["variant"] = v
		m["region"] = v
	}
	if v := strings.TrimSpace(*n.proxy); v != "" {
		m["proxy"] = v
	}
	if v := strings.TrimSpace(*n.baseURL); v != "" {
		m["base_url"] = v
	}
	if n.timeout != nil && *n.timeout > 0 {
		m["timeout"] = *n.timeout
	}
	return m
}

func main() {
	app := cli.NewApp(pluginName, "TraeWork 渠道插件（traework / traecode）", plugin.ModeCall)

	pCall := app.Cmd("call", "协议模式（core 调用）")
	dataCall := pCall.Str("data", "", "插件数据目录")

	pStdio := app.Cmd("stdio", "stdio 协议模式（调试）")
	dataStdio := pStdio.Str("data", "", "插件数据目录")

	pManifest := app.Cmd("manifest", "打印插件清单")
	manOut := pManifest.Str("out", "", "写入文件（默认 stdout）")

	pPing := app.Cmd("ping", "自检：逐个探测控制面方法")
	nPing := addNetFlags(pPing)

	pLogin := app.Cmd("login", "发起登录并轮询结果")
	nLogin := addNetFlags(pLogin)
	loginWait := pLogin.Int("wait", 300, "最长等待秒数")
	loginCallback := pLogin.Str("callback", "", "粘贴浏览器回调 URL（可选）")

	pStatus := app.Cmd("status", "查询登录流程状态")
	nStatus := addNetFlags(pStatus)
	statusFlow := pStatus.Str("flow", "", "流程 ID")

	pAccounts := app.Cmd("accounts", "列出账号")
	nAccounts := addNetFlags(pAccounts)

	pModels := app.Cmd("models", "列出模型")
	nModels := addNetFlags(pModels)

	pChat := app.Cmd("chat", "发起一次对话（流式打印）")
	nChat := addNetFlags(pChat)
	chatModel := pChat.Str("model", os.Getenv("WW3_MODEL"), "模型 ID")
	chatPrompt := pChat.Str("prompt", "你好", "用户输入")
	chatRaw := pChat.Str("raw", "", "完整 chat.generate 请求体 JSON（@file 可）")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		if err == cli.ErrHelp {
			return
		}
		fail("%v", err)
	}

	switch cmd.CmdName() {
	case plugin.ModeCall:
		dataDirGlobal = cli.ResolveData(*dataCall)
		serve(cmd.CmdName())
	case plugin.ModeStdio:
		dataDirGlobal = cli.ResolveData(*dataStdio)
		serve(cmd.CmdName())
	case "manifest":
		dispatchManifest(*manOut)
	case "ping":
		dataDirGlobal = cli.ResolveData(*nPing.data)
		dispatchPing(nPing.cfg())
	case "login":
		dataDirGlobal = cli.ResolveData(*nLogin.data)
		dispatchLogin(nLogin.cfg(), *loginWait, *loginCallback)
	case "status":
		dataDirGlobal = cli.ResolveData(*nStatus.data)
		dispatchStatus(*statusFlow, nStatus.cfg())
	case "accounts":
		dataDirGlobal = cli.ResolveData(*nAccounts.data)
		printResult(spawnInvoke("accounts.list", map[string]any{"config": nAccounts.cfg()}))
	case "models":
		dataDirGlobal = cli.ResolveData(*nModels.data)
		printResult(spawnInvoke("models.list", map[string]any{"config": nModels.cfg()}))
	case "chat":
		dataDirGlobal = cli.ResolveData(*nChat.data)
		dispatchChat(nChat.cfg(), *nChat.auth, *chatModel, *chatPrompt, *chatRaw)
	default:
		fail("未知命令: %s", cmd.CmdName())
	}
}

// serve 进入协议服务循环（真实入口）。
func serve(mode string) {
	log, err := cli.NewLogger(pluginName, cli.LogPath(dataDirGlobal, pluginName))
	if err != nil {
		fail("初始化日志失败: %v", err)
	}
	channel.Log = log
	defer log.Close()
	if err := channel.Serve(context.Background(), mode, dataDirGlobal, os.Stdin, os.Stdout); err != nil {
		log.Errorf("服务退出: %v", err)
		os.Exit(1)
	}
}

// ===== 子进程驱动（无 CLI 捷径）=====

// spawnInvoke 起自己的 `call` 子进程执行一次方法调用。
func spawnInvoke(method string, params any) *cli.Result {
	exe, err := os.Executable()
	if err != nil {
		fail("定位可执行文件失败: %v", err)
	}
	args := []string{plugin.ModeCall}
	if dataDirGlobal != "" {
		args = append(args, "--data", dataDirGlobal)
	}
	p, err := cli.Spawn(pluginName+"-call", exe, args, channel.Log)
	if err != nil {
		fail("启动子进程失败: %v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := p.InvokeSimple(ctx, method, params)
	if err != nil {
		fail("%s 失败: %v", method, err)
	}
	return res
}

// spawnStream 起子进程执行一次流式调用，正文实时打到 stdout。
func spawnStream(method string, params any, timeout time.Duration) *cli.Result {
	exe, err := os.Executable()
	if err != nil {
		fail("定位可执行文件失败: %v", err)
	}
	args := []string{plugin.ModeCall}
	if dataDirGlobal != "" {
		args = append(args, "--data", dataDirGlobal)
	}
	p, err := cli.Spawn(pluginName+"-call", exe, args, channel.Log)
	if err != nil {
		fail("启动子进程失败: %v", err)
	}
	defer p.Close()
	p.Out = os.Stdout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := p.Invoke(ctx, cli.Step{Method: method, Params: params, Stream: true})
	if err != nil || res == nil || !res.OK {
		os.Exit(2)
	}
	return res
}

// ===== 命令实现 =====

func dispatchManifest(out string) {
	b, err := json.MarshalIndent(channel.Manifest(), "", "  ")
	if err != nil {
		fail("序列化清单失败: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fail("写清单失败: %v", err)
	}
	fmt.Fprintf(os.Stderr, "已写入 %s\n", out)
}

func dispatchPing(cfg map[string]any) {
	probes := []string{
		"config.schema", "auth.methods", "accounts.list",
		"credits.get", "accounts.resource_detail", "stats.get",
	}
	bad := 0
	for _, m := range probes {
		res := spawnInvoke(m, map[string]any{"config": cfg, "days": 7})
		if res != nil && res.OK {
			fmt.Printf("ok   %-24s %s\n", m, truncateStr(cli.RedactJSON(res.Raw), 200))
			continue
		}
		bad++
		fmt.Printf("FAIL %-24s %v\n", m, resErr(res))
	}
	if bad > 0 {
		os.Exit(2)
	}
}

func dispatchLogin(cfg map[string]any, waitSec int, callback string) {
	res := spawnInvoke("auth.begin", map[string]any{
		"method": authflow.TypeLoopback,
		"region": regionOf(cfg),
		"config": cfg,
	})
	var br authflow.BeginResult
	if res != nil && res.Raw != "" {
		_ = json.Unmarshal([]byte(res.Raw), &br)
	}
	if br.AuthURL == "" {
		fail("未拿到授权地址: %v", resErr(res))
	}
	fmt.Fprintf(os.Stderr, "请在浏览器打开以下地址完成登录：\n\n  %s\n\n", br.AuthURL)
	if br.CallbackURL != "" {
		fmt.Fprintf(os.Stderr, "回调地址（若浏览器未自动完成，可把地址栏 URL 用 --callback 粘贴回来）：\n  %s\n\n", br.CallbackURL)
	}

	if strings.TrimSpace(callback) != "" {
		st := spawnStatus(br.FlowID, cfg, callback)
		printLoginOutcome(st)
		return
	}

	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	next := time.Duration(br.NextPollMS) * time.Millisecond
	if next <= 0 {
		next = 2 * time.Second
	}
	var lastState authflow.State
	for time.Now().Before(deadline) {
		time.Sleep(next)
		st := spawnStatusRaw(br.FlowID, cfg)
		if st.State != lastState && st.State != "" {
			fmt.Fprintf(os.Stderr, "状态: %s %s\n", st.State, st.Hint)
			lastState = st.State
		}
		if st.State == authflow.StateSuccess {
			printLoginOutcome(st)
			return
		}
		if st.State.Terminal() {
			printLoginOutcome(st)
			os.Exit(2)
		}
	}
	fail("登录等待超时（%d 秒）", waitSec)
}

func dispatchStatus(flowID string, cfg map[string]any) {
	if strings.TrimSpace(flowID) == "" {
		fail("缺少 --flow")
	}
	printLoginOutcome(spawnStatusRaw(flowID, cfg))
}

func spawnStatusRaw(flowID string, cfg map[string]any) *authflow.StatusResult {
	res := spawnInvoke("auth.status", map[string]any{"flow_id": flowID, "config": cfg})
	var st authflow.StatusResult
	if res != nil && res.Raw != "" {
		_ = json.Unmarshal([]byte(res.Raw), &st)
	}
	return &st
}

func spawnStatus(flowID string, cfg map[string]any, callback string) *authflow.StatusResult {
	res := spawnInvoke("auth.finish", map[string]any{"flow_id": flowID, "callback_url": callback, "config": cfg})
	var st authflow.StatusResult
	if res != nil && res.Raw != "" {
		_ = json.Unmarshal([]byte(res.Raw), &st)
	}
	return &st
}

func printLoginOutcome(st *authflow.StatusResult) {
	if st == nil {
		fail("登录结果为空")
	}
	if st.State == authflow.StateSuccess && st.Account != nil {
		fmt.Printf("登录成功\n  uid:      %s\n  nickname: %s\n  region:   %s\n  authFile: %s\n",
			st.Account.UID, st.Account.Nickname, st.Account.Region, st.Account.AuthFile)
		return
	}
	msg := st.Hint
	if st.Error != nil && st.Error.Message != "" {
		msg = st.Error.Message
	}
	fail("登录未完成（%s）：%s", st.State, msg)
}

func dispatchChat(cfg map[string]any, authName, model, prompt, rawBody string) {
	variant := tw.ResolveVariant(asString(cfg["variant"]), regionOf(cfg))
	prof := tw.ProfileFor(variant)

	var req chat.GenerateRequest
	if raw := strings.TrimSpace(cli.ConfigArg(rawBody)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			fail("解析 --raw 失败: %v", err)
		}
	}
	req.Stream = true
	if req.AccountUID == "" {
		req.AccountUID = strings.TrimSpace(authName)
	}
	if req.ConversationID == "" {
		req.ConversationID = fmt.Sprintf("cli-%d", os.Getpid())
	}
	if req.ChannelConfig == nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			fail("序列化渠道配置失败: %v", err)
		}
		req.ChannelConfig = b
	}

	m := strings.TrimSpace(model)
	if m == "" {
		m = strings.TrimSpace(req.Model)
	}
	if m == "" {
		fail("缺少 --model")
	}
	if !strings.HasPrefix(m, prof.Prefix) {
		// 裸模型名补上本变体前缀；跨变体前缀留给 worker 报 ModelBlocked。
		if !strings.HasPrefix(m, tw.ProfileFor(tw.VariantCode).Prefix) &&
			!strings.HasPrefix(m, tw.ProfileFor(tw.VariantWork).Prefix) {
			m = prof.Prefix + m
		}
	}
	req.Model = m

	if len(req.Messages) == 0 {
		req.Messages = []chat.ChatMessage{{Role: "user", Content: prompt}}
	}

	res := spawnStream("chat.generate", req, 30*time.Minute)
	if res != nil && res.Text == "" {
		fmt.Fprintln(os.Stderr)
	}
}

// ===== 小工具 =====

func printResult(res *cli.Result) {
	if res == nil {
		fail("空结果")
	}
	if !res.OK {
		fail("%v", resErr(res))
	}
	if res.Raw != "" {
		fmt.Println(cli.RedactJSON(res.Raw))
		return
	}
	b, _ := json.Marshal(res.Result)
	fmt.Println(cli.RedactJSON(string(b)))
}

func resErr(res *cli.Result) any {
	if res != nil && res.Err != nil {
		return fmt.Sprintf("kind=%s scope=%s msg=%s", res.Err.Kind, res.Err.Scope, res.Err.Message)
	}
	return "调用失败"
}

// regionOf 从配置里取变体/区域。
func regionOf(cfg map[string]any) string {
	return firstNonEmpty(asString(cfg["variant"]), asString(cfg["region"]))
}

// asString 把任意值转为字符串。
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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

// truncateStr 按 rune 截断并加省略号。
func truncateStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fail 打印到 stderr 并退出（码 2，便于脚本区分）。
func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, pluginName+": "+format+"\n", a...)
	os.Exit(2)
}
