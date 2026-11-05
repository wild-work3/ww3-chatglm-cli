package channel

// 控制面方法：models.list / config.schema / accounts.* / auth.* / stats.get。
// 形状以 ww3 protocol 的 authflow / meta 结构为准（glm-ext 同款语义）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	"wild-work3/chatglm-cli/internal/protocol/authflow"
	"wild-work3/chatglm-cli/internal/protocol/jsonl"
	"wild-work3/chatglm-cli/internal/protocol/meta"
	"wild-work3/chatglm-cli/internal/protocol/stat"
)

// ModelsList 返回静态模型表（上游无公开模型接口，静态表即权威；glm-ext 同款结论）。
func (ch *Channel) ModelsList(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	models := make([]meta.ModelInfo, 0, len(staticModels))
	for _, m := range staticModels {
		models = append(models, meta.ModelInfo{ID: m.ID, Name: m.Name,
			Capabilities: meta.Capabilities2{Reasoning: m.Reasoning}})
	}
	return mustJSON(map[string]any{"models": models, "default": "glm/chatglm:moe_53f"}), nil
}

// ConfigSchema 返回 config.json / --config / channel_config 的字段说明（含约束注释）。
func (ch *Channel) ConfigSchema(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	return mustJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"auth":     map[string]any{"type": "string", "title": "凭据标识", "description": "auths 目录下的凭据文件名（去 .json）"},
			"base_url": map[string]any{"type": "string", "title": "上游基址", "default": "https://chatglm.cn", "description": "一般不改；联调/自建反代时覆盖"},
			"timeout":  map[string]any{"type": "integer", "title": "非流式请求超时（秒）", "default": 60},
			"proxy": map[string]any{"type": "string", "title": "代理",
				"description": "HTTP(S) 代理，如 http://127.0.0.1:7890；留空则跟随环境变量"},
			"refresh_token": map[string]any{"type": "string", "title": "refresh_token", "format": "password",
				"description": "浏览器 Cookie 里的 chatglm_refresh_token（F12 → Application → Cookies）。保存时立即验活并落盘，不进配置文件"},
			"allow_image_host": map[string]any{"type": "boolean", "title": "允许上传参考图到第三方图床", "default": false,
				"description": "上游生图只收公网 URL：多张参考图或 base64 参考图必须上传到第三方图床换取直链。开启即表示知悉并同意把参考图上传至该服务；关闭时此类请求直接报错（单张公网 URL 参考图不受影响）"},
		},
		"note": "多轮对话与工具调用不受支持（上游只接受单条 user 提问；续轮请传 conversation_id）",
	}), nil
}

// AccountsList 列举 auths 目录全部凭据（离线，不碰网络）。
func (ch *Channel) AccountsList(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	entries, bad, err := ch.store.List()
	if err != nil {
		return nil, errf("server", "channel", "读取 auths 失败: %v", err)
	}
	accounts := make([]meta.AccountInfo, 0, len(entries))
	for _, e := range entries {
		status := "ok"
		if e.Auth.RefreshToken == "" {
			status = "session_dead" // 没有根凭据，无法续期
		}
		accounts = append(accounts, meta.AccountInfo{
			UID: e.Auth.UserID, Nickname: e.Auth.Nickname, Status: status,
			LoginKind: "form", ExpiresAt: expText(e.Auth.ExpiresAt),
			ChannelID: ID, Region: ID, Prefixes: []string{ID}, AuthFile: e.Name,
		})
	}
	out := map[string]any{"accounts": accounts}
	if len(bad) > 0 {
		out["bad_files"] = bad
	}
	return mustJSON(out), nil
}

// expText Unix 秒 → RFC3339；0 返回空串（未知）。
func expText(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// AccountsRemove 删除凭据文件。params: {auth_file|uid}。
func (ch *Channel) AccountsRemove(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		AuthFile string `json:"auth_file"`
		UID      string `json:"uid"`
	}
	_ = json.Unmarshal(params, &p)
	name := p.AuthFile
	if name == "" && p.UID != "" {
		e, err := ch.store.FindByUID(p.UID)
		if err != nil {
			return nil, errf("not_found", "account", "未找到 uid=%s 的凭据", p.UID)
		}
		name = e.Name
	}
	if name == "" {
		return nil, errf("bad_params", "request", "需要 auth_file 或 uid")
	}
	if err := ch.store.Delete(name); err != nil {
		return nil, errf("server", "channel", "删除凭据失败: %v", err)
	}
	return mustJSON(map[string]any{"removed": name}), nil
}

// ── 额度与积分（W3-14 ResourceItem / W3-17 契约） ─────────

// ResourceDetail 额度明细。params: {uid, config}。
// 回**裸数组** []meta.ResourceItem（core app.js showDetail 直接 map，W3-17）。
func (ch *Channel) ResourceDetail(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		UID    string          `json:"uid"`
		Config json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(params, &p)
	c, err := ch.Client(ctx, p.UID, p.Config)
	if err != nil {
		return nil, mapClientErr(err)
	}
	_, items, err := c.UserResourceDetail(ctx)
	if err != nil {
		return nil, mapClientErr(err)
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(items), nil
}

// CreditsGet 积分余额。params: {uid, config}。
// balance 允许 null（未上报 ≠ 0）：账号缺失/上游失败都回 null 而不报错，
// 否则面板每 10 分钟的定时刷新会满屏红字（W3-14 §5）。
func (ch *Channel) CreditsGet(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		UID    string          `json:"uid"`
		Config json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(params, &p)
	out := map[string]any{"balance": nil, "items": []meta.ResourceItem{}, "unit": "credits"}
	c, err := ch.Client(ctx, p.UID, p.Config)
	if err != nil {
		return mustJSON(out), nil
	}
	bal, items, err := c.UserResourceDetail(ctx)
	if err != nil {
		return mustJSON(out), nil
	}
	out["balance"], out["items"] = bal, items
	return mustJSON(out), nil
}

// ── 保活与签到（W3-19 账号生命周期） ────────────────────

// AccountRefresh 刷新凭据（保活续期）。params: {uid, config}。
// 刷新会轮换 refresh_token，必须立即落盘（NewW3Credential 已注入 saveFn）。
func (ch *Channel) AccountRefresh(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		UID    string          `json:"uid"`
		Config json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(params, &p)
	a, name, e := ch.pickAuth(p.UID, p.Config)
	if e != nil {
		return nil, e
	}
	if a.RefreshToken == "" {
		return nil, errf("auth", "account", "凭据 %s 缺少 refresh_token，无法刷新（需重新登录）", name)
	}
	c, err := ch.Client(ctx, name, p.Config)
	if err != nil {
		return nil, mapClientErr(err)
	}
	if err := c.EnsureToken(true); err != nil {
		return nil, mapClientErr(err)
	}
	msg := "已刷新"
	if score, rerr := c.UserResource(ctx); rerr == nil {
		msg = fmt.Sprintf("已刷新，剩余 %d 积分", score)
	}
	return mustJSON(map[string]any{"ok": true, "message": msg}), nil
}

// AccountCheckin 签到（= 保活对话 + 领每日登录积分）。params: {uid, config}，uid 缺省则全账号。
// 只有**全部失败**才返回错误：部分成功也应当视为成功（否则面板会误导用户去重新登录）。
func (ch *Channel) AccountCheckin(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		UID    string          `json:"uid"`
		Config json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(params, &p)
	entries, _, err := ch.store.List()
	if err != nil {
		return nil, errf("server", "channel", "读取 auths 失败: %v", err)
	}
	targets := make([]glm.W3Entry, 0, len(entries))
	for _, e := range entries {
		if p.UID != "" && e.Name != p.UID && e.Auth.UserID != p.UID {
			continue
		}
		targets = append(targets, e)
	}
	if len(targets) == 0 {
		return nil, errf("auth", "account", "该渠道尚未登录，请先在面板完成登录")
	}
	var msgs []string
	failed := 0
	for _, e := range targets {
		c, cerr := ch.Client(ctx, e.Name, p.Config)
		if cerr != nil {
			failed++
			msgs = append(msgs, fmt.Sprintf("%s: %v", e.Name, cerr))
			continue
		}
		// 过期就先刷新（签到请求本身也会带 EnsureToken，这里提前刷新便于失败定位）
		if e.Auth.AccessExpired() && e.Auth.RefreshToken != "" {
			if rerr := c.EnsureToken(true); rerr != nil {
				failed++
				msgs = append(msgs, fmt.Sprintf("%s: 刷新失败: %v", e.Name, rerr))
				continue
			}
		}
		msg, derr := c.DailyCheckinReport(ctx)
		if derr != nil {
			failed++
			msgs = append(msgs, fmt.Sprintf("%s: %s", e.Name, msg))
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Name, msg))
	}
	joined := strings.Join(msgs, "; ")
	if failed == len(targets) {
		return nil, errf("client", "account", "签到失败: %s", joined)
	}
	return mustJSON(map[string]any{"ok": true, "message": joined}), nil
}

// pickAuth 选凭据：uid 优先（凭据名或上游 uid），否则第一个未过期，再否则第一个。
func (ch *Channel) pickAuth(uid string, raw json.RawMessage) (*glm.W3Auth, string, *jsonl.ErrObj) {
	entries, _, err := ch.store.List()
	if err != nil {
		return nil, "", errf("server", "channel", "读取 auths 失败: %v", err)
	}
	if len(entries) == 0 {
		return nil, "", errf("auth", "account", "该渠道尚未登录，请先在面板完成登录")
	}
	name := uid
	if name == "" {
		name = ch.config(raw).Auth
	}
	if name != "" {
		for _, e := range entries {
			if e.Name == name || e.Auth.UserID == name {
				return e.Auth, e.Name, nil
			}
		}
		return nil, "", errf("not_found", "account", "未找到凭据 %q", name)
	}
	for _, e := range entries {
		if !e.Auth.AccessExpired() {
			return e.Auth, e.Name, nil
		}
	}
	return entries[0].Auth, entries[0].Name, nil
}

// ── 配置（W3-35：配置由 core 持有，插件侧回当前生效值） ──

// ConfigGet 返回当前生效配置（不含凭据）。
func (ch *Channel) ConfigGet(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	cfg := ch.config(nil)
	out := map[string]any{
		"base_url":         cfg.BaseURL,
		"timeout":          int(cfg.Timeout / time.Second),
		"auth":             cfg.Auth,
		"proxy":            cfg.Proxy,
		"allow_image_host": cfg.AllowImageHost,
	}
	return mustJSON(map[string]any{"config": out}), nil
}

// ConfigSet 合并写入配置。params: {config:{...}} 或直接 {...}。
// 便捷路径：带了 refresh_token 就立即验活并落盘（不进 config.json）。
func (ch *Channel) ConfigSet(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var w struct {
		Config map[string]any `json:"config"`
	}
	_ = json.Unmarshal(params, &w)
	in := w.Config
	if in == nil {
		_ = json.Unmarshal(params, &in)
	}
	if in == nil {
		return nil, errf("bad_params", "request", "缺少 config 对象")
	}
	// refresh_token 是凭据：绝不进 config.json（会被轮换，留下就是一把已作废的旧钥匙）
	rt, _ := in["refresh_token"].(string)
	delete(in, "refresh_token")
	for k, v := range in {
		ch.setCfg(k, v)
	}
	if err := ch.saveConfig(); err != nil {
		return nil, errf("bad_params", "request", "保存配置失败: %v", err)
	}
	if strings.TrimSpace(rt) != "" {
		name, e := ch.adoptRefreshToken(ctx, rt)
		if e != nil {
			return nil, e
		}
		return mustJSON(map[string]any{"ok": true, "auth_file": name}), nil
	}
	return mustJSON(map[string]any{"ok": true}), nil
}

// adoptRefreshToken 校验并落盘一把 refresh_token（config.set 的便捷路径，省一次 auth.* 往返）。
func (ch *Channel) adoptRefreshToken(ctx context.Context, rt string) (string, *jsonl.ErrObj) {
	acct, err := glm.ValidateRefreshToken(ctx, rt)
	if err != nil {
		return "", mapClientErr(err)
	}
	if strings.TrimSpace(acct.UID) == "" {
		return "", errf("bad_params", "account", "未能取得账号 ID，请确认 refresh_token 完整")
	}
	a := &glm.W3Auth{AccessToken: acct.AccessToken, RefreshToken: acct.RefreshToken,
		ExpiresAt: acct.ExpiresAt, UserID: acct.UID, Nickname: acct.Nickname}
	if a.RefreshToken == "" {
		a.RefreshToken = rt
	}
	name, serr := ch.store.Save(a, glm.AuthFileName(a.UserID))
	if serr != nil {
		return "", errf("account_fault", "account", "凭据落盘失败: %v", serr)
	}
	return name, nil
}

// ── authflow（form 范式） ────────────────────────────────

// AuthMethods 本渠道唯一支持粘贴 refresh_token（form）。
func (ch *Channel) AuthMethods(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	methods := []authflow.Method{{
		ID: glm.MethodForm, Type: authflow.TypeForm, Label: "粘贴 refresh_token", Default: true, Region: ID,
		Description: "在浏览器登录 chatglm.cn 后，从 Cookie 复制 chatglm_refresh_token 提交（访客 token 无法使用，须登录账号）",
		Fields: map[string]any{
			"refresh_token": map[string]any{"type": "string", "title": "chatglm_refresh_token", "format": "password", "required": true},
		},
	}}
	return mustJSON(authflow.MethodsResult{Region: ID, Methods: methods,
		Note: "refresh_token 每次刷新都会轮换，插件会自动落盘轮换值"}), nil
}

type flowParams struct {
	FlowID string            `json:"flow_id"`
	Method string            `json:"method"`
	Region string            `json:"region"`
	Values map[string]string `json:"values"`
	// 顶层 refresh_token 是 auth.finish 的简化姿势（core/CLI 直接粘 token 免包 values）
	RefreshToken string `json:"refresh_token"`
}

// AuthBegin 发起 form 登录：同步返回 waiting_input。
func (ch *Channel) AuthBegin(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	f, err := ch.logins.Begin(p.Method)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return mustJSON(authflow.BeginResult{FlowID: f.ID, Type: authflow.TypeForm, Method: f.Method,
		State: f.State, Prompt: f.Hint, ExpiresAt: expText(f.ExpiresAt)}), nil
}

// flowResult 把 LoginFlow 摊平成 StatusResult（错误进 Error 字段，Go error 恒 nil —— 契约惯例）。
func flowResult(f *glm.LoginFlow) json.RawMessage {
	r := authflow.StatusResult{State: authflowState(f.State), Hint: f.Hint, NextPollMS: 0}
	if f.ErrorKind != "" {
		r.Error = &authflow.Error{Kind: f.ErrorKind, Message: f.ErrorMsg}
	}
	if f.State == "success" {
		acct := f.Account()
		r.Account = &authflow.Account{UID: f.UID, Nickname: f.Nickname, ChannelID: acct["channel_id"].(string),
			LoginKind: acct["login_kind"].(string), AuthFile: f.AuthFile}
	}
	return mustJSON(r)
}

// authflowState 内部状态串 → authflow.State 常量。
func authflowState(s string) authflow.State {
	switch s {
	case "waiting_input":
		return authflow.StateWaitingInput
	case "success":
		return authflow.StateSuccess
	case "failed":
		return authflow.StateFailed
	case "expired":
		return authflow.StateExpired
	case "cancelled":
		return authflow.StateCancelled
	}
	return authflow.StatePending
}

// AuthStatus 查询登录进度。
func (ch *Channel) AuthStatus(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	f, err := ch.logins.Status(p.FlowID)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// AuthFinish 提交 refresh_token（values.refresh_token 或顶层 refresh_token）。
func (ch *Channel) AuthFinish(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	if p.Values == nil {
		p.Values = map[string]string{}
	}
	if p.RefreshToken != "" {
		p.Values["refresh_token"] = p.RefreshToken
	}
	f, err := ch.logins.Finish(ctx, p.FlowID, p.Values, ch.store)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// AuthCancel 取消登录。
func (ch *Channel) AuthCancel(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p flowParams
	_ = json.Unmarshal(params, &p)
	if p.FlowID == "" {
		return nil, errf("bad_params", "request", "缺少 flow_id")
	}
	f, err := ch.logins.Cancel(p.FlowID)
	if err != nil {
		return nil, mapClientErr(err)
	}
	return flowResult(f), nil
}

// StatsGet 返回流水汇总。params: {days 缺省 7}。
// 回**裸 stat.Summary**（core views.go 直接 json.Unmarshal 到 stat.Summary，
// 套一层 {days,summary,daily} 会让面板读到全零）。
func (ch *Channel) StatsGet(ctx context.Context, params jsonl.RawMessage) (json.RawMessage, *jsonl.ErrObj) {
	var p struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Days <= 0 {
		p.Days = 7
	}
	if ch.stat == nil {
		return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
	}
	recs := ch.stat.Query(p.Days)
	sort.Slice(recs, func(i, j int) bool { return recs[i].TS < recs[j].TS })
	return mustJSON(stat.Summarize(recs)), nil
}

// RecordChat 便利命令（直连内核）的 chat 流水；token 未上报记 -1。
// 成功记录不写 err_kind：ErrKindAndScope(nil) 会回默认值 "server"，写进去会让
// 「成功」的流水带一个错误分类（面板按 err_kind 归类会误读）。
func (ch *Channel) RecordChat(model string, ok bool, err error, start time.Time) {
	rec := stat.Record{Kind: stat.KindChat, Account: ch.Auth, Model: model, OK: ok,
		PromptTokens: -1, CompletionTokens: -1, TotalTokens: -1,
		TimeMs: int(time.Since(start).Milliseconds())}
	if !ok {
		rec.ErrKind, _ = glm.ErrKindAndScope(err)
	}
	ch.Record(rec)
}
