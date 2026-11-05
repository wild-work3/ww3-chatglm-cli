// Package cli 是 glm2cli 的命令层：ww3 PLUGIN_CLI 契约的外壳 + glm 私有便利命令。
//
// 双重身份（用户拍板）：既可独立使用，也直接作为 ww3 的渠道插件（命令名/开关名/方法名
// 与 ww3/docs/PLUGIN_CLI.md 逐条对齐，见 cmd_channel.go）。参数风格硬约定（W3-21）：
//   - 开关一律双短横线全名（--prompt），无单短横线简写、无位置参数（命令名除外）；
//   - 必需命令 call/stdio/manifest/ping 的行为与 core 预期逐字节对齐；
//   - stdout 只出 JSONL 数据，日志/进度全走 stderr 与 <data>/logs/glm2cli.log。
//
// 本包是「web 类逆向渠道 cli 的标准契约」参考实现（chatgpt-cli 等照此移植）。
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	pcli "wild-work3/chatglm-cli/internal/protocol/cli"

	"wild-work3/chatglm-cli/internal/channel"
	"wild-work3/chatglm-cli/internal/glm"
)

// VERSION 与 go.mod 保持一致（发版时同步）。
const VERSION = "2.0.0"

// env 是一次进程生命周期的共享状态。
type env struct {
	ctx   context.Context
	out   *json.Encoder
	start time.Time
}

// common 是渠道类命令共用的开关（PLUGIN_CLI §3；call/stdio 只有 --data）。
// 全部存 flagset 指针：注册时值还没解析，存值拷贝永远是空串。
type common struct {
	data, auth, config, base, proxy *string
	timeout                         *int
	debug, insecure                 *bool
}

func addCommon(c *pcli.Command) *common {
	return &common{
		data:     c.Str("data", "", "数据目录（缺省 $WW3_DATA → exe 所在目录）"),
		auth:     c.Str("auth", "", "凭据标识 = auths 目录下凭据文件名（去 .json）"),
		config:   c.Str("config", "", "渠道配置 JSON 或 @file（覆盖 <data>/config.json）"),
		base:     c.Str("base-url", "", "上游基址覆盖（一般不改）"),
		proxy:    c.Str("proxy", "", "代理地址（如 http://127.0.0.1:10809）；缺省跟随 HTTPS_PROXY"),
		timeout:  c.Int("timeout", 0, "非流式请求超时（秒）；流式不受限"),
		debug:    c.Bool("debug", false, "详细日志到 stderr"),
		insecure: c.Bool("insecure", false, "跳过 TLS 校验（仅本地抓包调试）"),
	}
}

// open 构造渠道对象（凭据/统计/登录编排都挂在它上面）。
func (e *env) open(cf *common) (*channel.Channel, error) {
	ch, err := channel.New(pcli.ResolveData(*cf.data))
	if err != nil {
		return nil, err
	}
	ch.Auth, ch.Base, ch.Debug, ch.Insecure = *cf.auth, *cf.base, *cf.debug, *cf.insecure
	ch.Proxy = *cf.proxy
	if *cf.timeout > 0 {
		ch.Timeout = time.Duration(*cf.timeout) * time.Second
	}
	if err := ch.SetConfig(*cf.config); err != nil {
		return nil, err
	}
	return ch, nil
}

// client 快捷：默认渠道配置下拿一个内核客户端（便利命令直连用；IPC 命令走 registry）。
func (e *env) client(cf *common) (*channel.Channel, *glm.Client, error) {
	ch, err := e.open(cf)
	if err != nil {
		return nil, nil, err
	}
	c, err := ch.Client(e.ctx, "", nil)
	return ch, c, err
}

// ── 输出原语（glm2cli 事件契约，与 internal/glm/testdata/contract.json 对齐） ──

func (e *env) ev(ev *glm.Event) {
	ev.V = glm.EV
	if err := e.out.Encode(ev); err != nil {
		fmt.Fprintf(os.Stderr, "写 stdout 失败: %v\n", err)
	}
}

func (e *env) result(cmd string, data any) {
	e.ev(&glm.Event{Type: "result", Cmd: cmd, Data: mustJSON(data)})
}

func (e *env) passthrough(cmd string, raw json.RawMessage) {
	e.ev(&glm.Event{Type: "result", Cmd: cmd, Data: raw})
}

func (e *env) fail(err error) {
	e.ev((&glm.Event{}).Err(err))
}

// step 发一条 item 进度事件。
func (e *env) step(cmd, step string, extra map[string]any) {
	m := map[string]any{"step": step}
	for k, v := range extra {
		m[k] = v
	}
	e.ev(&glm.Event{Type: "item", Cmd: cmd, Data: mustJSON(m)})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 本地构造的数据不可能序列化失败
	}
	return b
}

// code 退出码 = glm.Error.Code 数值（contract.json exit_codes）。
func code(err error) int {
	if ge, ok := err.(*glm.Error); ok {
		return int(ge.Code)
	}
	return int(glm.CUpstream)
}

// ── chat 流式共用（glm2cli 事件契约；channel.ChatGenerate 是 ww3 归一事件版） ──

// streamChatEvents 执行一次对话流：透传全部事件（meta 在拿到首帧后先发 ⇒
// conversation_id 必是懒创建后的真实值）；调用方负责 done 与收尾。
func (e *env) streamChatEvents(c *glm.Client, opt glm.ChatOpt, cmd string, newConv bool) (*glm.Agg, error) {
	resp, err := c.Chat(opt)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var metaSent bool
	return glm.ReadStream(resp.Body, func(ev *glm.Event) { e.ev(ev) }, func(g *glm.Agg) {
		if metaSent {
			return
		}
		metaSent = true
		e.ev(&glm.Event{Type: "meta", Cmd: cmd, ConversationID: g.ConversationID, MessageID: g.MessageID,
			AgentID: opt.Agent, Model: opt.Model, New: newConv, TS: e.start.Unix()})
	})
}

// stopBestEffort 流未正常收尾时尽力中止服务端生成（失败忽略，不掩盖主错误）。
func (e *env) stopBestEffort(c *glm.Client, mid string) {
	if mid == "" || !glm.CIDPattern.MatchString(mid) {
		return
	}
	_, _ = c.WithContext(context.Background()).StopStream(mid)
}

// ── 小工具 ──

// readPrompt 提示词来源：--prompt 优先，"-" 强制 stdin；缺省时 stdin 非 TTY 自动读。
func readPrompt(flagVal string) (string, error) {
	if flagVal == "-" {
		return readStdin()
	}
	if flagVal != "" {
		return flagVal, nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return "", glm.NewErr(glm.CUsage, "没有提示词：用 --prompt 传入，或从 stdin 管道输入")
	}
	return readStdin()
}

func readStdin() (string, error) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return "", glm.NewErr(glm.CUsage, "读 stdin 失败: %v", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", glm.NewErr(glm.CUsage, "stdin 是空的")
	}
	return s, nil
}

// resolveAgent 把别名展开成 assistant_id（glm 私有便利层；--ratio 是生图显式信号）。
func resolveAgent(alias, ratio string) (string, error) {
	m := map[string]string{"main": glm.DEFAULT_ASSISTANT, "drawing": glm.ASSISTANT_DRAWING,
		"read": glm.ASSISTANT_READ, "video": glm.ASSISTANT_VIDEO, "study": glm.ASSISTANT_STUDY,
		"search": channel.AssistantSearch, "ppt": channel.AssistantPPT}
	if ratio != "" && (alias == "" || alias == "main") {
		return glm.ASSISTANT_DRAWING, nil
	}
	if id, ok := m[alias]; ok {
		return id, nil
	}
	if alias == "" || glm.CIDPattern.MatchString(alias) {
		return alias, nil
	}
	return "", glm.NewErr(glm.CUsage, "--agent 未知: %q（可用 main|drawing|read|video|study|search|ppt 或 24 位 hex）", alias)
}

func mask(tok string) string {
	if tok == "" {
		return "(empty)"
	}
	return fmt.Sprintf("%.8s…(len=%d)", tok, len(tok))
}

func goVersion() string { return runtime.Version() }

// handler 是单个命令的执行体，返回退出码。
type handler func(c *pcli.Command) int

// Main 是进程入口（main.go 调用），返回退出码。
func Main(argv []string) int {
	app := pcli.NewApp("glm2cli", "智谱清言(chatglm.cn) 私有接口 CLI / ww3 渠道插件（glm）", "")
	handlers := registerAll(app)
	cmd, err := app.Parse(argv)
	if err == pcli.ErrHelp {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "glm2cli: %v\n", err)
		return 2
	}
	h, ok := handlers[cmd.Name]
	if !ok {
		fmt.Fprintf(os.Stderr, "glm2cli: 命令 %q 未注册\n", cmd.Name)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mainCtx = ctx
	e := &env{ctx: ctx, start: time.Now()}
	e.out = json.NewEncoder(os.Stdout)
	e.out.SetEscapeHTML(false) // 正文里的 < > & 不能被转义，否则代码/Markdown 被改坏
	return h(cmd)
}
