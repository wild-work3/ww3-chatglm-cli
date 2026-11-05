package channel

// chat.generate —— IPC_CHAT 契约的 glm 实现。
//
// glm 私有约束（与本仓内核契约一致）：
//   - 不支持工具调用：req.Tools 直接忽略（用户拍板）；
//   - 不做多轮：messages 里只取最后一个 user 提示词，system/assistant 一律丢弃（用户拍板）；
//   - temperature/max_tokens/top_p 上游不收，静默忽略；
//   - usage 无 token 计数，只有耗时（time_ms/first_token_ms），token 缺省即「未提供」。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	"wild-work3/chatglm-cli/internal/protocol/chat"
	"wild-work3/chatglm-cli/internal/protocol/jsonl"
	"wild-work3/chatglm-cli/internal/protocol/plugin"
	"wild-work3/chatglm-cli/internal/protocol/stat"
)

// pluginEmit 短别名。
type pluginEmit = plugin.Emit

// glm 专属扩展 assistant（glm-ext 实测表；内核 constants 里没有的在这里补）。
const (
	AssistantSearch = "659e54b1b8006379b4b2abd6" // AI 搜索
	AssistantPPT    = "670e3c3e119b48fe5a851149" // AI PPT
)

// ResolveModel 把 chat.generate 的 model（含 glm/ 前缀）解析为
// (assistant_id, chat_mode, selected_model)。未知值当 selected_model 原样透传，
// 由上游裁决（与内核 --model 行为一致）。
func ResolveModel(model string) (agent, mode, selected string) {
	if model == "" {
		return glm.DEFAULT_ASSISTANT, "", glm.DEFAULT_MODEL
	}
	if glm.CIDPattern.MatchString(model) { // 24hex 直接当 assistant_id
		return model, "", ""
	}
	m := model
	if i := strings.Index(model, "/"); i >= 0 { // 剥渠道前缀
		if model[:i] != ID {
			return "", "", "" // 非本渠道前缀，调用方报 model_blocked
		}
		m = model[i+1:]
	}
	switch m {
	case "chatglm", "chatglm:moe_53f", "glm", "default":
		return glm.DEFAULT_ASSISTANT, "", "moe_53f"
	case "chatglm-think", "chatglm-think:moe_53f", "think", "thinking":
		return glm.DEFAULT_ASSISTANT, "zero", "moe_53f" // 深度思考：chat_mode=zero [实证 glm-ext]
	case "chatglm-deepresearch", "chatglm-deepresearch:moe_53f", "deepresearch":
		return glm.DEFAULT_ASSISTANT, "deep_research", "moe_53f" // 沉思
	case "chatglm-search", "search":
		return AssistantSearch, "", "ai-search"
	case "chatglm-ppt", "ppt":
		return AssistantPPT, "", "all-tools-glms-glms-v2"
	case "chatglm-video", "video":
		return glm.ASSISTANT_VIDEO, "", "all-tools-glms-glms-v2"
	}
	return glm.DEFAULT_ASSISTANT, "", m
}

// staticModels 是 models.list 的权威来源（上游无公开模型接口，静态表即契约；glm-ext 同款）。
var staticModels = []metaModel{
	{ID: "glm/chatglm:moe_53f", Name: "智谱清言"},
	{ID: "glm/chatglm-think:moe_53f", Name: "智谱清言·深度思考", Reasoning: true},
	{ID: "glm/chatglm-deepresearch:moe_53f", Name: "智谱清言·沉思", Reasoning: true},
	{ID: "glm/chatglm-search", Name: "AI 搜索"},
	{ID: "glm/chatglm-ppt", Name: "AI PPT"},
	{ID: "glm/chatglm-video", Name: "AI 视频"},
}

type metaModel struct {
	ID, Name  string
	Reasoning bool
}

// lastUserPrompt 取 messages 里最后一个 user 消息的纯文本（content 是 string 或 parts 数组）。
func lastUserPrompt(msgs []chat.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			return strings.TrimSpace(c)
		case []any: // parts：[{type:"text",text:"..."}...]；图片部件 chat 通道收不了，忽略
			var b strings.Builder
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
					if s, ok := pm["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
			return strings.TrimSpace(b.String())
		case nil:
			return ""
		default: // 其它 JSON 形态，兜底重序列化取文本
			if raw, err := json.Marshal(c); err == nil {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					return strings.TrimSpace(s)
				}
			}
			return ""
		}
	}
	return ""
}

// ChatGenerate 处理 chat.generate：流式转发 ww3 归一事件，result = usage 对象（glm-ext 同款）。
func (ch *Channel) ChatGenerate(ctx context.Context, params jsonl.RawMessage, emit pluginEmit) (json.RawMessage, *jsonl.ErrObj) {
	start := time.Now()
	var req chat.GenerateRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, errf("bad_params", "request", "chat.generate 参数解析失败: %v", err)
	}
	agent, mode, selected := ResolveModel(req.Model)
	if agent == "" { // 带了别的渠道前缀
		return nil, errf("model_blocked", "model", "模型 %q 不属于 %s 渠道", req.Model, ID)
	}
	// 上游只接受单条 user 提问（历史由上游按 conversation_id 自己维护）。
	// 带 assistant 历史的多轮请求必须显式拒绝，不能静默丢历史（用户会误以为上下文生效）。
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			return nil, errf("bad_params", "request",
				"不支持多轮对话：上游只接受单条 user 提问，请勿携带 assistant 历史（续轮请传 conversation_id）")
		}
	}
	prompt := lastUserPrompt(req.Messages)
	if prompt == "" {
		return nil, errf("bad_params", "request", "messages 里没有可用的 user 文本消息（本插件只取最后一个 user 提示词）")
	}
	if req.ConversationID != "" && !glm.CIDPattern.MatchString(req.ConversationID) {
		return nil, errf("bad_params", "request", "conversation_id %q 不是 24 位 hex", req.ConversationID)
	}
	effort := req.ReasoningEffort
	if effort == "" && req.Thinking {
		effort = "thinking"
	}
	c, err := ch.Client(ctx, req.AccountUID, req.ChannelConfig)
	if err != nil {
		return nil, mapClientErr(err)
	}

	opt := glm.ChatOpt{Agent: agent, CID: req.ConversationID, Prompt: prompt,
		Model: selected, Mode: mode, Effort: effort, Web: true} // 联网默认开（内核默认）
	resp, err := c.Chat(opt)
	if err != nil {
		return nil, mapClientErr(err)
	}
	defer resp.Body.Close()

	var firstAt time.Time
	terminated := false
	fail := func(e *jsonl.ErrObj) (json.RawMessage, *jsonl.ErrObj) { // 终止只一次：error 事件 + ErrObj
		if !terminated {
			terminated = true
			_ = emit("error", map[string]any{"kind": e.Kind, "scope": e.Scope, "message": e.Message,
				"retryable": e.Retryable})
		}
		return nil, e
	}

	agg, rerr := glm.ReadStream(resp.Body, func(ev *glm.Event) {
		switch ev.Type {
		case "meta": // 元信息只采集，不透传给 core
		case "text", "reasoning":
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			name := "text.delta"
			if ev.Type == "reasoning" {
				name = "reasoning.delta"
			}
			_ = emit(name, map[string]string{"content": ev.Delta})
		case "image": // 画图 agent 的产物：IPC_CHAT 无图片事件，按 markdown 正文兜底
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			_ = emit("text.delta", map[string]string{"content": "![图片](" + ev.URL + ")"})
		case "error":
			// 流中错误帧（上游业务错误）：翻译成 errkind
			fail(errObjFromStream(ev))
		}
	}, nil)
	if rerr != nil { // 读流失败（网络断等）
		return fail(mapClientErr(rerr))
	}
	if agg.Err != nil { // 截断/流中错误帧必须报错（铁律：绝不伪造收尾帧）
		return fail(errObjFromAgg(agg.Err))
	}

	// 正常收尾：finish_reason → usage → completed（终止只一次）。
	// usage 只带耗时字段：glm 上游无 token 计数，不能发 0（契约 null/缺省=未提供≠0）。
	_ = emit("finish_reason", map[string]string{"reason": "stop"})
	timeMs := time.Since(start).Milliseconds()
	firstMs := int64(0)
	if !firstAt.IsZero() {
		firstMs = firstAt.Sub(start).Milliseconds()
	}
	usage := map[string]any{"time_ms": timeMs, "first_token_ms": firstMs}
	_ = emit("usage", usage)
	_ = emit("completed", map[string]any{})

	rec := stat.Record{Kind: "chat", Account: req.AccountUID, Model: "glm/" + selected,
		OK: true, TimeMs: int(timeMs), FirstTokenMs: int(firstMs),
		PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1, CachedTokens: -1}
	ch.Record(rec)
	return mustJSON(usage), nil
}

// errObjFromStream 把 SSE error 帧（glm Event）翻译成 ww3 ErrObj。
func errObjFromStream(ev *glm.Event) *jsonl.ErrObj {
	e := &jsonl.ErrObj{Kind: "server", Scope: "channel", Message: ev.Message}
	if ev.Code != "" {
		e.Message = "[" + ev.Code + "] " + ev.Message
		if ev.Code == "40012" {
			e.Kind, e.Scope, e.Retryable = "soft_rate", "account", boolPtr(true)
		}
	}
	if ev.Detail != "" {
		e.Cause = ev.Detail
	}
	return e
}

// errObjFromAgg 把 Agg.Err（SSE 错误帧）翻译成 ww3 ErrObj。
func errObjFromAgg(e *glm.ErrObj) *jsonl.ErrObj {
	out := &jsonl.ErrObj{Kind: "server", Scope: "channel", Message: e.ErrMsg}
	if e.ErrorCode == 40012 {
		out.Kind, out.Scope, out.Retryable = "soft_rate", "account", boolPtr(true)
	}
	out.Message = fmt.Sprintf("[%d] %s", e.ErrorCode, e.ErrMsg)
	return out
}

// mapClientErr 内核错误 → ww3 ErrObj（映射表见 glm.ErrKindAndScope）。
func mapClientErr(err error) *jsonl.ErrObj {
	kind, scope := glm.ErrKindAndScope(err)
	e := &jsonl.ErrObj{Kind: kind, Scope: scope, Message: err.Error()}
	if ge, ok := err.(*glm.Error); ok && ge.Code.Retryable() {
		e.Retryable = boolPtr(true)
	}
	return e
}

func boolPtr(b bool) *bool { return &b }
