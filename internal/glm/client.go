package glm

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const (
	acceptJSON = "application/json, text/plain, */*"
	acceptSSE  = "text/event-stream"
)

// Client 是清言私有接口的 HTTP 客户端。一个进程一份凭据。
//
// 与 Python 版的差异（都是刻意的）：
//   - 只做 IDE 通道：不拼 refer__991；
//   - **不做退避重试**：IDE 链路实测 100% 通过，重试只会掩盖问题（fail early）；
//   - 流式请求不设任何超时（只跟随 ctx 取消，ctx 由信号处理控制）。
type Client struct {
	Cred    *Credential
	Base    string
	Timeout time.Duration // 非流式读超时；流式忽略
	Debug   bool
	Warn    func(string) // 非致命提示（CLI 侧接成 warn 事件 / stderr 日志）

	// IdleTimeout 聊天 SSE 流中空闲超时（Stream 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration

	ctx     context.Context
	hc      *http.Client
	jarURL  *url.URL
	LastRID string // 最近一次请求的 X-Request-Id，供 meta/error 事件对齐日志
}

// Opt 配置客户端。
type Opt struct {
	Base     string
	Insecure bool
	Timeout  time.Duration
	Debug    bool
	Proxy    string // 空则跟随环境变量（http.ProxyFromEnvironment）
}

func NewClient(ctx context.Context, cred *Credential, opt Opt) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opt.Base == "" {
		opt.Base = BASE
	}
	if opt.Timeout <= 0 {
		opt.Timeout = READ_TIMEOUT * time.Second
	}
	u, err := url.Parse(strings.TrimRight(opt.Base, "/"))
	if err != nil {
		return nil, NewErr(CUsage, "base 地址无效: %v", err)
	}
	tr := &http.Transport{
		// 流式长回答靠 idle 看门狗兜底，这里只管「等响应头」与连接层
		ResponseHeaderTimeout: 3 * time.Minute,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
	}
	// 代理：显式配置优先，未配置时跟随环境变量（与 glm-ext 同口径）。
	// IDE 通道实测直连稳定，代理只在国内网络异常时按需开启。
	if p := strings.TrimSpace(opt.Proxy); p != "" {
		if u, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	if opt.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 仅调试用（配合本地抓包看明文）
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, NewErr(CUpstream, "初始化 cookie jar 失败: %v", err)
	}
	return &Client{Cred: cred, Base: u.Scheme + "://" + u.Host, Timeout: opt.Timeout,
		Debug: opt.Debug, IdleTimeout: DefaultTimeout, ctx: ctx,
		hc: &http.Client{Jar: jar, Transport: tr}, jarURL: u}, nil
}

// Ctx 暴露请求上下文（CLI 用它区分「被 Ctrl-C」与「上游断流」）。
func (c *Client) Ctx() context.Context { return c.ctx }

// WithContext 返回一个只换 ctx 的浅拷贝客户端（中断后还要发 stop_stream，原 ctx 已取消）。
func (c *Client) WithContext(ctx context.Context) *Client {
	n := *c
	n.ctx = ctx
	return &n
}

// ── Cookie ─────────────────────────────────────────────────
// 业务 cookie 进 jar（不设显式 Cookie 头）：清言走阿里云 WAF，POST 类接口依赖
// acw_tc/cdn_sec_tc 链路，显式 Cookie 头会覆盖 jar 导致持续 40012 [实证 PROTOCOL §1.4]。

// warn 输出非致命提示（不污染 stdout 的数据流）。
func (c *Client) warn(f string, a ...any) {
	if c.Warn != nil {
		c.Warn(fmt.Sprintf(f, a...))
	}
}

// syncCookies 把凭据写进 jar（Cookie 头交给 jar，绝不手写，原因见上）。
func (c *Client) syncCookies() {
	pairs := [][2]string{{"chatglm_token", c.Cred.AccessToken},
		{"chatglm_refresh_token", c.Cred.RefreshToken}, {"chatglm_user_id", c.Cred.UserID}}
	for k, v := range c.Cred.ExtraCookies {
		if !contains(COOKIE_KEYS, k) {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	var ck []*http.Cookie
	for _, kv := range pairs {
		if kv[1] == "" {
			continue
		}
		ck = append(ck, &http.Cookie{Name: kv[0], Value: kv[1], Domain: "chatglm.cn", Path: "/"})
	}
	c.hc.Jar.SetCookies(c.jarURL, ck) // cookiejar.Jar.SetCookies 永不返回错误（只在 URL 非法时忽略）
}

// ── 请求底层 ───────────────────────────────────────────────
func (c *Client) build(ctx context.Context, method, path string, query map[string]string, body any, accept string) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, NewErr(CUsage, "请求体编码失败: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	u := c.Base + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, NewErr(CUsage, "构造请求失败: %v", err)
	}
	h := Headers(c.Cred.AccessToken, accept, c.Cred.DeviceID)
	c.LastRID = h["X-Request-Id"]
	for k, v := range h {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	return req, nil
}

// raw 发一次请求（不解信封、不解压），返回响应；调用方负责关闭 Body。
func (c *Client) raw(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &Error{Code: CUpstream, Message: "请求失败: " + err.Error(), RequestID: c.LastRID}
	}
	return resp, nil
}

// Do 发一次非流式请求，返回信封里的 result（原样 JSON）。401 会自愈：强制刷新后重试一次。
//
// ⚠️ 只有这里用 Timeout；流式（Stream）**不设超时**，否则长回答会被掐断且没有终止帧。
func (c *Client) Do(method, path string, query map[string]string, body any) (json.RawMessage, error) {
	if err := c.EnsureToken(false); err != nil {
		return nil, err
	}
	c.syncCookies()
	ctx, cancel := context.WithTimeout(c.ctx, c.Timeout)
	defer cancel()
	req, err := c.build(ctx, method, path, query, body, acceptJSON)
	if err != nil {
		return nil, err
	}
	resp, err := c.raw(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized { // 本地 exp 未到但上游已作废 ⇒ 靠 401 自愈
		resp.Body.Close()
		if err := c.EnsureToken(true); err != nil {
			return nil, err
		}
		if req, err = c.build(ctx, method, path, query, body, acceptJSON); err != nil {
			return nil, err
		}
		if resp, err = c.raw(req); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(decodeBody(resp), 8<<20))
	if err != nil {
		return nil, &Error{Code: CUpstream, Message: "读响应失败: " + err.Error(), RequestID: c.LastRID}
	}
	return c.envelope(resp, rawBody)
}

// doRaw 发一次非流式请求并返回**原始响应体与状态码**，不校验业务信封。
//
// 用途：调用方需要区分「HTTP 失败」与「非 0 业务码属于正常语义」（如 daily_login_score
// 的 status=10001「今日已领取」）。401 自愈重试一次，与 Do 一致。
func (c *Client) doRaw(method, path string, query map[string]string, body any, accept string) ([]byte, int, error) {
	if err := c.EnsureToken(false); err != nil {
		return nil, 0, err
	}
	c.syncCookies()
	ctx, cancel := context.WithTimeout(c.ctx, c.Timeout)
	defer cancel()
	req, err := c.build(ctx, method, path, query, body, accept)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.raw(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if err := c.EnsureToken(true); err != nil {
			return nil, 0, err
		}
		if req, err = c.build(ctx, method, path, query, body, accept); err != nil {
			return nil, 0, err
		}
		if resp, err = c.raw(req); err != nil {
			return nil, 0, err
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(decodeBody(resp), 8<<20))
	if err != nil {
		return nil, resp.StatusCode, &Error{Code: CUpstream, Message: "读响应失败: " + err.Error(), RequestID: c.LastRID}
	}
	return raw, resp.StatusCode, nil
}

// envelope 校验 HTTP 状态与业务信封，返回 result。
func (c *Client) envelope(resp *http.Response, body []byte) (json.RawMessage, error) {
	var env struct {
		Status  json.RawMessage `json:"status"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
		Rid     string          `json:"rid"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &Error{Code: CUpstream, Message: "响应不是合法 JSON（可能被 WAF 拦截页）",
			HTTPStatus: resp.StatusCode, Detail: snippet(string(body)), RequestID: c.LastRID}
	}
	biz := 0
	fmt.Sscanf(string(env.Status), "%d", &biz)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Error{Code: CRateLimited, Message: "被限流: " + env.Message, HTTPStatus: resp.StatusCode, BizStatus: biz, RequestID: c.LastRID}
	case biz == STATUS_TRANSIENT:
		return nil, &Error{Code: CTransient, HTTPStatus: resp.StatusCode, BizStatus: biz,
			Message: "风控瞬时拒绝（本版不做退避重试：IDE 链路实测不出现）；请稍后重跑或降低频率: " + env.Message,
			Detail:  snippet(string(body)), RequestID: c.LastRID}
	case biz == AUTH_STATUS_1 || biz == AUTH_STATUS_2 || resp.StatusCode == http.StatusUnauthorized:
		return nil, &Error{Code: CAuth, HTTPStatus: resp.StatusCode, BizStatus: biz,
			Message: fmt.Sprintf("登录态失效: %s", env.Message), Detail: snippet(string(body)), RequestID: c.LastRID}
	case resp.StatusCode >= 400:
		return nil, &Error{Code: CUpstream, HTTPStatus: resp.StatusCode, BizStatus: biz,
			Message: fmt.Sprintf("上游错误: %s", orText(env.Message, "http error")), Detail: snippet(string(body)), RequestID: c.LastRID}
	case biz != 0:
		return nil, &Error{Code: CUpstream, HTTPStatus: resp.StatusCode, BizStatus: biz,
			Message: fmt.Sprintf("业务错误: %s", orText(env.Message, "unknown error")), Detail: snippet(string(body)), RequestID: c.LastRID}
	}
	return env.Result, nil
}

// ── 流式 ───────────────────────────────────────────────────
// Stream 建立 SSE 连接。调用方负责逐帧消费（sse.go）并关闭响应。
// ⚠️ 不设任何 http.Client/请求超时：长回答会被掐断且没有终止帧（硬规则 2/3）。
func (c *Client) Stream(body any) (*http.Response, error) {
	if err := c.EnsureToken(false); err != nil {
		return nil, err
	}
	c.syncCookies()
	// 流式：不设总超时（长回答会被掐断且没有终止帧），改用「挂 ctx + 读 body 后包空闲监控」
	ctx, cancel := context.WithCancel(c.ctx)
	req, err := c.build(ctx, http.MethodPost, EP_CHAT, nil, body, acceptSSE)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := c.raw(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		defer resp.Body.Close()
		cancel()
		b, _ := io.ReadAll(io.LimitReader(decodeBody(resp), 64<<10))
		_, err := c.envelope(resp, b) // 先过一遍信封判定，把鉴权/限流类错误翻译成对应 Code
		if err != nil {
			return nil, err
		}
		return nil, &Error{Code: CUpstream, Message: fmt.Sprintf("预期 SSE，实际 HTTP %d / Content-Type=%q",
			resp.StatusCode, resp.Header.Get("Content-Type")), HTTPStatus: resp.StatusCode,
			Detail: snippet(string(b)), RequestID: c.LastRID}
	}
	// cancel 的所有权交给监控 body：Close 时取消（含空闲超时主动取消）
	resp.Body = Monitor(resp.Body, c.IdleTimeout, cancel)
	return resp, nil
}

// ── 凭据 ───────────────────────────────────────────────────
// EnsureToken 保证 access 可用；过期或 force 时刷新，并把**轮换后的 refresh_token 落盘**。
func (c *Client) EnsureToken(force bool) error {
	if !force && !c.Cred.AccessExpired() {
		return nil
	}
	if err := c.Refresh(); err != nil {
		return err
	}
	c.syncCookies()
	return nil
}

// Refresh 用 refresh_token 换 access_token。上游会**同时轮换 refresh_token**，
// 因此换完必须落盘；落盘失败立刻报错（内存与磁盘不一致时，下次启动会拿已作废的 refresh）。
func (c *Client) Refresh() error {
	h := Headers(c.Cred.RefreshToken, acceptJSON, c.Cred.DeviceID) // 只用根凭据：access 可能已失效
	c.LastRID = h["X-Request-Id"]
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.Base+EP_REFRESH, strings.NewReader("{}"))
	if err != nil {
		return NewErr(CUsage, "构造 refresh 请求失败: %v", err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	// 刷新用独立 client：不共享业务 jar，避免把可能已失效的 access 带进去
	rc := &http.Client{Jar: c.hc.Jar, Transport: c.hc.Transport, Timeout: c.Timeout}
	resp, err := rc.Do(req)
	if err != nil {
		return &Error{Code: CUpstream, Message: "refresh 请求失败: " + err.Error(), RequestID: c.LastRID}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(decodeBody(resp), 1<<20))
	res, err := c.envelope(resp, body)
	if err != nil {
		if resp.StatusCode == http.StatusForbidden {
			return &Error{Code: CAuth, Message: "refresh 被拒（HTTP 403）：refresh_token 可能已失效",
				HTTPStatus: resp.StatusCode, RequestID: c.LastRID}
		}
		return err
	}
	var out struct {
		UserID       string `json:"user_id"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IsGuest      bool   `json:"is_guest"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return &Error{Code: CUpstream, Message: "refresh 响应结构异常", Detail: snippet(string(res)), RequestID: c.LastRID}
	}
	if out.IsGuest {
		return NewErr(CAuth, "访客账号不可用：请用真实账号登录清言后重新取 refresh_token")
	}
	if out.AccessToken == "" {
		return NewErr(CAuth, "refresh 未返回 access_token")
	}
	c.Cred.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		c.Cred.RefreshToken = out.RefreshToken
	}
	if out.UserID != "" {
		c.Cred.UserID = out.UserID
	}
	c.Cred.AbsorbJWT()
	if c.Cred.Source != "" && c.Cred.Source != "env" {
		if err := c.Cred.Save(); err != nil {
			return NewErr(CAuth, "轮换后的 refresh_token 落盘失败（内存与磁盘不一致，下次启动会拿旧值）: %v", err)
		}
	} else {
		c.warn("凭据来自环境变量，轮换后的 refresh_token 无法落盘 ⇒ 下次启动必须重新取 Cookie")
	}
	return nil
}

// ── 小工具 ─────────────────────────────────────────────────
func decodeBody(resp *http.Response) io.Reader {
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		if r, err := gzip.NewReader(resp.Body); err == nil {
			return r
		}
	case "deflate":
		return flate.NewReader(resp.Body)
	}
	return resp.Body
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func orText(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
