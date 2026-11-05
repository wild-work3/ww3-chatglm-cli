package glm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credential 是一份账号凭据。字段与 Python 侧 glm_credential.json 一一对应（可互读互写）。
//
// 根凭据是 RefreshToken（唯一能换 access 的东西）；AccessToken 可空，首次请求前自动换取。
type Credential struct {
	RefreshToken string            `json:"refresh_token"`
	AccessToken  string            `json:"access_token"`
	DeviceID     string            `json:"device_id"`
	UserID       string            `json:"user_id"`
	ExtraCookies map[string]string `json:"extra_cookies,omitempty"`

	Source string `json:"-"` // 文件路径；"env" 表示来源环境变量（不可落盘）
	origin string // 本次刷新所依据的 refresh（写盘前做乐观比对）
	// saveFn 非空时接管落盘（ww3 authstore 嵌套形由 channel 层注入），空则按旧扁平形写。
	saveFn      func(*Credential) error
	nickname    string                     // ww3 authstore 冗余昵称（随轮换写回）
	w3ExpiresAt int64                      // ww3 authstore 的显式 expiresAt（Unix 秒，0=用 JWT）
	unknown     map[string]json.RawMessage // 保留文件里本工具不认识的键，避免覆盖 Python 侧数据
}

// JWTPayload 只 base64 解 JWT 的 payload，**不验签、不校验**；失败返回 nil（调用方据此判「这不是 JWT」）。
func JWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	p := parts[1]
	if pad := len(p) % 4; pad != 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func jwtInt(token, key string) int64 {
	if p := JWTPayload(token); p != nil {
		switch v := p[key].(type) {
		case float64:
			return int64(v)
		case string:
			var n int64
			fmt.Sscanf(v, "%d", &n)
			return n
		}
	}
	return 0
}

func jwtStr(token, key string) string {
	if p := JWTPayload(token); p != nil {
		if s, ok := p[key].(string); ok {
			return s
		}
	}
	return ""
}

// AccessExp / RefreshExp 是 JWT 里的 exp（Unix 秒）；非 JWT 或缺失返回 0。
func (c *Credential) AccessExp() int64  { return jwtInt(c.AccessToken, "exp") }
func (c *Credential) RefreshExp() int64 { return jwtInt(c.RefreshToken, "exp") }

// AccessExpired 判断 access 是否需要刷新（默认提前 AUTH_SKEW 秒，避免边界失败）。
// ww3 authstore 的显式 expiresAt 优先于 JWT（与 W3Auth.AccessExpired 同口径）——
// 夹具与旧版凭据常把 token 写成非 JWT，只看 JWT 会把长期凭据误判成过期。
func (c *Credential) AccessExpired() bool {
	if c.AccessToken == "" {
		return true
	}
	if c.w3ExpiresAt > 0 {
		return c.w3ExpiresAt-time.Now().Unix() < AUTH_SKEW
	}
	return c.AccessExp() <= time.Now().Unix()+AUTH_SKEW
}

// AbsorbJWT 用 refresh（优先）或 access 的 payload 补齐 UserID / DeviceID。
func (c *Credential) AbsorbJWT() {
	for _, tok := range []string{c.RefreshToken, c.AccessToken} {
		if JWTPayload(tok) == nil {
			continue
		}
		if c.UserID == "" {
			c.UserID = jwtStr(tok, "uid")
		}
		if c.DeviceID == "" {
			c.DeviceID = jwtStr(tok, "device_id")
		}
		return
	}
}

// ParseCookie 把整条 Cookie 拆成键值，丢弃 WAF/CDN 浏览器指纹项 [实证 PROTOCOL §1.4]。
func ParseCookie(cookie string) map[string]string {
	out := map[string]string{}
	for _, seg := range strings.Split(cookie, ";") {
		k, v, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if isWAFCookie(k) {
			continue
		}
		out[k] = v
	}
	return out
}

func isWAFCookie(k string) bool {
	for _, w := range WAF_COOKIE_KEYS {
		if k == w {
			return true
		}
	}
	for _, p := range WAF_COOKIE_PREFIXES {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// MakeCredential 从「整条 cookie + 零散字段」构造凭据（对齐 Python make_credential）。
func MakeCredential(refresh, access, deviceID, userID, cookie string) *Credential {
	ck := ParseCookie(cookie)
	c := &Credential{ExtraCookies: map[string]string{}}
	if v, ok := ck["chatglm_refresh_token"]; ok && refresh == "" {
		refresh = v
	}
	if v, ok := ck["chatglm_token"]; ok && access == "" {
		access = v
	}
	if v, ok := ck["chatglm_user_id"]; ok && userID == "" {
		userID = v
	}
	for k, v := range ck {
		if !contains(COOKIE_KEYS, k) {
			c.ExtraCookies[k] = v
		}
	}
	c.RefreshToken, c.AccessToken = strings.TrimSpace(refresh), strings.TrimSpace(access)
	c.DeviceID, c.UserID = strings.TrimSpace(deviceID), strings.TrimSpace(userID)
	c.AbsorbJWT()
	return c
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// DefaultCredPath 按优先级返回凭据文件路径：显式 → $GLM_CREDENTIAL_FILE → ./glm_credential.json → ~/.glm2cli/credential.json。
// 全都不存在时返回空串（调用方回落到环境变量）。
func DefaultCredPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv(ENV_CREDENTIAL_FILE); p != "" {
		return p
	}
	for _, p := range []string{"glm_credential.json", filepath.Join(homeDir(), ".glm2cli", "credential.json")} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

// LoadCredential 加载凭据：文件优先，缺字段用环境变量兜底；无文件时纯环境变量（Source="env"，不可落盘）。
func LoadCredential(path string) (*Credential, error) {
	path = DefaultCredPath(path)
	if path == "" {
		c := MakeCredential(os.Getenv(ENV_REFRESH_TOKEN), os.Getenv(ENV_ACCESS_TOKEN),
			os.Getenv(ENV_DEVICE_ID), os.Getenv(ENV_USER_ID), os.Getenv(ENV_COOKIE))
		c.Source = "env"
		if c.RefreshToken == "" {
			return nil, fmt.Errorf("缺少 refresh_token：请设置 %s 或提供凭据文件（--help 看取凭据步骤）", ENV_REFRESH_TOKEN)
		}
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读凭据文件失败: %w", err)
	}
	c, err := parseCredBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("凭据文件 %s 无效: %w", path, err)
	}
	// 文件缺字段时用 env 兜底（与 Python 侧一致）
	if c.RefreshToken == "" {
		c.RefreshToken = os.Getenv(ENV_REFRESH_TOKEN)
	}
	if c.AccessToken == "" {
		c.AccessToken = os.Getenv(ENV_ACCESS_TOKEN)
	}
	if c.DeviceID == "" {
		c.DeviceID = os.Getenv(ENV_DEVICE_ID)
	}
	if c.UserID == "" {
		c.UserID = os.Getenv(ENV_USER_ID)
	}
	c.AbsorbJWT()
	if c.RefreshToken == "" {
		return nil, fmt.Errorf("凭据文件 %s 里没有 refresh_token", path)
	}
	c.Source, c.origin = path, c.RefreshToken
	return c, nil
}

// parseCredBytes 支持三种写法：JSON 对象 / 整条 cookie / 裸 refresh token（与 Python _load_file 同规则）。
func parseCredBytes(raw []byte) (*Credential, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, fmt.Errorf("文件为空")
	}
	if strings.HasPrefix(s, "{") {
		var d map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &d); err != nil {
			return nil, err
		}
		str := func(k string) string {
			var v string
			if r, ok := d[k]; ok {
				_ = json.Unmarshal(r, &v)
			}
			return v
		}
		ck := map[string]string{}
		_ = json.Unmarshal(d["extra_cookies"], &ck)
		c := MakeCredential(str("refresh_token"), str("access_token"), str("device_id"), str("user_id"), str("cookie"))
		for k, v := range ck {
			if _, dup := c.ExtraCookies[k]; !dup {
				c.ExtraCookies[k] = v
			}
		}
		delete(d, "refresh_token")
		delete(d, "access_token")
		delete(d, "device_id")
		delete(d, "user_id")
		delete(d, "extra_cookies")
		delete(d, "cookie")
		c.unknown = d // 未知键原样保留，写回时不丢
		return c, nil
	}
	if strings.Contains(s, "=") {
		return MakeCredential("", "", "", "", s), nil // 整条 cookie
	}
	return MakeCredential(s, "", "", "", ""), nil // 裸 token
}

// Save 原子写回凭据文件（tmp + rename）。
//
// ⚠️ refresh_token 每次刷新都会轮换 [实证 PROTOCOL §1.3 铁律 1]：**必须落盘**，
// 否则下次启动拿旧值 → 账号失效。落盘失败必须显式报错，不允许静默吞掉。
func (c *Credential) Save() error {
	if c.saveFn != nil { // ww3 authstore 模式：嵌套形落盘由 channel 层实现
		return c.saveFn(c)
	}
	if c.Source == "" || c.Source == "env" {
		return nil // 环境变量模式无法落盘（调用方负责提示）
	}
	// 乐观比对：磁盘上的 refresh 若已不是本次刷新的依据，说明别的进程（或另一个 glm2cli）刷过 ⇒ 放弃覆盖。
	if cur, err := parseCredBytes(mustRead(c.Source)); err == nil && cur.RefreshToken != "" &&
		c.origin != "" && cur.RefreshToken != c.origin {
		return fmt.Errorf("凭据文件 %s 已被其它进程轮换（refresh_token 与本次刷新依据不一致），放弃写回；请重跑命令", c.Source)
	}
	b, err := json.MarshalIndent(c.diskForm(), "", "  ")
	if err != nil {
		return err
	}
	tmp := c.Source + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Source)
}

// diskForm 拼出写盘结构：已知字段 + 保留的未知键。
func (c *Credential) diskForm() map[string]any {
	m := map[string]any{"refresh_token": c.RefreshToken, "access_token": c.AccessToken,
		"device_id": c.DeviceID, "user_id": c.UserID, "extra_cookies": c.ExtraCookies}
	for k, v := range c.unknown {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return m
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}
