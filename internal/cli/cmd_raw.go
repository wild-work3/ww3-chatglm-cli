// cmd_raw.go —— raw 逃生门、schema 自描述与 version。
package cli

import (
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/channel"
	"wild-work3/chatglm-cli/internal/glm"
	pcli "wild-work3/chatglm-cli/internal/protocol/cli"
)

func registerRaw(app *pcli.App, reg map[string]handler) {
	{
		c := app.Cmd("raw", "直调上游接口（逃生门；路径含 /chatglm/ 时自动截取）")
		method := c.Str("method", "GET", "GET|POST")
		path := c.Str("path", "", "接口路径，如 backend-api/assistant/stream")
		body := c.Str("body", "", "POST JSON 体；\"-\" 读 stdin")
		params := c.StrList("param", "query 参数 k=v，可重复")
		cf := addCommon(c)
		reg["raw"] = func(*pcli.Command) int { return runRaw(method, path, body, params, cf) }
	}
	{
		_ = app.Cmd("schema", "输出本 CLI 的机器可读契约（命令/事件/退出码）")
		reg["schema"] = func(*pcli.Command) int { return runSchema() }
	}
	{
		_ = app.Cmd("version", "版本信息")
		reg["version"] = func(*pcli.Command) int { return runVersion() }
	}
}

func runRaw(method, path, body *string, params *[]string, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	m := strings.ToUpper(*method)
	if m != "GET" && m != "POST" {
		err := glm.NewErr(glm.CUsage, "--method 只支持 GET|POST")
		e.fail(err)
		return code(err)
	}
	p := *path
	if p == "" {
		err := glm.NewErr(glm.CUsage, "缺少 --path")
		e.fail(err)
		return code(err)
	}
	// git-bash MSYS 会把 /xxx 换成盘符路径，含 /chatglm/ 时截取子串自愈
	if i := strings.Index(p, "/chatglm/"); i >= 0 {
		p = p[i:]
	} else if strings.HasPrefix(p, "chatglm/") {
		p = "/" + p
	}
	q := map[string]string{}
	for _, kv := range *params {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			err := glm.NewErr(glm.CUsage, "--param 应为 k=v：%q", kv)
			e.fail(err)
			return code(err)
		}
		q[k] = v
	}
	var payload any
	if b := *body; b != "" {
		var bs []byte
		if b == "-" {
			bs, err = readStdinBytes()
			if err != nil {
				e.fail(err)
				return code(err)
			}
		} else {
			bs = []byte(b)
		}
		var v any
		if err := json.Unmarshal(bs, &v); err != nil {
			err := glm.NewErr(glm.CUsage, "--body 不是合法 JSON: %v", err)
			e.fail(err)
			return code(err)
		}
		payload = v
	}
	res, err := c.Do(m, p, q, payload)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	e.passthrough("raw", res)
	return 0
}

func runVersion() int {
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	e.result("version", map[string]any{"version": VERSION, "go": runtime.Version()})
	return 0
}

func readStdinBytes() ([]byte, error) { return io.ReadAll(io.LimitReader(os.Stdin, 8<<20)) }

func runSchema() int {
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	e.result("schema", map[string]any{
		"version":  VERSION,
		"protocol": "ww3-plugin-cli/1 + glm2cli 事件契约",
		"commands": []string{
			// 契约命令（PLUGIN_CLI §2）
			"call", "stdio", "manifest", "ping",
			// 可选能力命令
			"images", "systemone",
			// glm 便利命令
			"chat", "models", "agents", "user", "accounts", "auth", "login",
			"conv-list", "conv-show", "conv-history", "conv-rename", "conv-delete", "conv-stop", "conv-read",
			"raw", "schema", "version",
		},
		"events": []string{"meta", "text", "reasoning", "image", "item", "result", "done", "warn", "error"},
		"exit_codes": map[string]int{
			"ok": 0, "upstream_error": 1, "usage_error": 2, "auth_error": 3,
			"transient_rejection": 4, "stream_truncated": 5, "rate_limited": 6, "not_proven": 7,
		},
		"retryable":  []string{"transient_rejection", "stream_truncated", "rate_limited"},
		"channel_id": channel.ID,
		"abilities": map[string]any{
			// glm 只有 web 凭据通道（无匿名模式）；能力如实声明，不虚构
			"web": map[string]any{
				"trigger": "--auth <凭据标识>（或 --auth auto 自动挑第一条未过期）",
				"chat":    true, "search": true, "thinking": true,
				"image": true, "image_ref": true,
				"conv_manage": true, "agents": true,
				"models": "glm/ 前缀静态表（glm2cli models），无公开模型目录接口",
			},
		},
		"quirks": []string{
			"请求带 X-Sign 签名（上游 WAF），签名算法内嵌于 internal/glm/sign",
			"会话/消息 ID 一律 24 位 hex（--cid/--message-id 本地预校验，不合法直接 usage_error）",
			"conv-history 的 output 是终态 SSE 帧，解析后才是正文；--raw 附带原始帧",
			"凭据是 JWT（payload exp 即过期时间，客户端不验签）；过期自动走 user/refresh",
			"conv-list 内部翻页一次取全量（recent_list），--limit/--offset 在过滤后截断",
			"多轮对话靠 conversation_id 续轮：上游自己维护历史，messages 只收单条 user 提问",
		},
		"ipc_methods": "见 `glm2cli manifest` 的 capabilities",
		"gaps": []string{
			"不支持工具调用（chat.generate 收到 tools 直接忽略）",
			"不支持多轮（带 assistant 历史的请求显式拒绝 bad_params；续轮靠 conversation_id）",
			"无 token 用量（上游不返回任何 token/计费字段，usage 只有 time_ms/first_token_ms）",
			"systemone.decide 未实现（not_supported）",
		},
	})
	return 0
}
