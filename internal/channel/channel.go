// Package channel 是 glm2cli 的 ww3 渠道插件层：把 ww3 PLUGIN_CLI / IPC_CHAT 契约
// （chat.generate / images.generate + 控制面方法）桥接到 internal/glm 内核。
//
// 分层纪律：本包不做协议发明，所有行为以 ww3/docs/PLUGIN_CLI.md、IPC_CHAT.md、
// PLUGIN_PROTOCOL.md 为准；glm 私有语义（assistant_id/chat_mode 等）只在模型映射处出现。
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	"wild-work3/chatglm-cli/internal/protocol/jsonl"
	"wild-work3/chatglm-cli/internal/protocol/stat"
)

// 插件标识（manifest 与模型前缀）。
const (
	ID      = "glm"
	Name    = "智谱清言"
	Version = "2.0.0"
)

// Errf 快捷构造 jsonl.ErrObj。
func errf(kind, scope, format string, a ...any) *jsonl.ErrObj {
	return &jsonl.ErrObj{Kind: kind, Scope: scope, Message: fmt.Sprintf(format, a...)}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// Channel 是一次插件进程会话的根对象：持有数据目录、authstore、登录编排与统计器。
type Channel struct {
	Data     string
	Auth     string        // --auth 凭据标识（缺省自动选择）
	Base     string        // --base-url 覆盖
	Proxy    string        // --proxy 覆盖（空则跟随 HTTPS_PROXY 环境变量）
	Timeout  time.Duration // --timeout 覆盖（非流式请求）
	Debug    bool
	Insecure bool

	store  *glm.W3Store
	logins *glm.LoginManager
	stat   *stat.Store
	cfg    map[string]any // config.json + --config 合并结果
}

// New 打开数据目录（懒建 auths/logs/stat 子目录）并加载 <data>/config.json。
func New(dataDir string) (*Channel, error) {
	ch := &Channel{Data: dataDir, store: glm.NewW3Store(dataDir), logins: glm.NewLoginManager(dataDir)}
	for _, sub := range []string{"auths", "logs", "stat"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	ch.stat = stat.Open(dataDir) // stat.Open 内部自拼 <dir>/stat 子目录
	// config.json 缺失/损坏都容忍：插件应能在空配置下运行
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			ch.cfg = m
		}
	}
	return ch, nil
}

// SetConfig 合并 --config（json 串或 @file）进基础配置（非空键覆盖）。
func (ch *Channel) SetConfig(configJSON string) error {
	if configJSON == "" {
		return nil
	}
	b := []byte(configJSON)
	if len(configJSON) > 1 && configJSON[0] == '@' {
		var err error
		if b, err = os.ReadFile(configJSON[1:]); err != nil {
			return glm.NewErr(glm.CUsage, "读取 --config 文件失败: %v", err)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return glm.NewErr(glm.CUsage, "--config 不是合法 JSON 对象: %v", err)
	}
	if ch.cfg == nil {
		ch.cfg = m
		return nil
	}
	for k, v := range m {
		if s, ok := v.(string); !ok || s != "" { // 空串视为未设置
			ch.cfg[k] = v
		}
	}
	return nil
}

// Client 按请求构造内核客户端：uid（params.account_uid）> channel_config.auth > --auth >
// auths 目录里第一个未过期凭据 > 旧版 glm_credential.json / GLM_* 环境变量（单机直用模式）。
func (ch *Channel) Client(ctx context.Context, uid string, channelCfg json.RawMessage) (*glm.Client, error) {
	cfg := ch.config(channelCfg)
	name := uid
	if name == "" {
		name = cfg.Auth
	}
	var cred *glm.Credential
	switch {
	case name != "": // 指定凭据标识：只认 <data>/auths/<name>.json
		a, err := ch.store.Load(name)
		if err != nil {
			return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录（凭据 %q 不可用：%v）；先用 glm2cli login 或 auth.begin 落盘凭据", name, err)
		}
		cred = glm.NewW3Credential(ch.store, name, a)
	default: // 未指定：auths 里挑第一个未过期的，再回落旧版凭据文件
		entries, _, err := ch.store.List()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.Auth.AccessExpired() {
				cred = glm.NewW3Credential(ch.store, e.Name, e.Auth)
				break
			}
		}
		if cred == nil { // 旧版单文件（兼容 glm2cli 独立用法）
			var err error
			if cred, err = glm.LoadCredential(""); err != nil {
				// 旧路径的错误是 fmt 错（非 *glm.Error），统一升为 auth/account 语义
				return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录：%v", err)
			}
			if cred == nil {
				return nil, glm.NewErr(glm.CAuth, "该渠道尚未登录：auths 目录无凭据且未找到旧版 glm_credential.json")
			}
		}
	}
	return glm.NewClient(ctx, cred, glm.Opt{Base: cfg.BaseURL, Timeout: cfg.Timeout,
		Proxy: cfg.Proxy, Debug: ch.Debug, Insecure: ch.Insecure})
}

// Config 是本渠道的生效配置（优先级：调用方 params.config / channel_config >
// --config / <data>/config.json > 命令行开关 > 默认值）。
type Config struct {
	BaseURL        string
	Timeout        time.Duration
	Auth           string // 指定凭据标识
	Proxy          string // 空=跟随环境变量
	AllowImageHost bool   // 允许把参考图上传第三方图床（W3-07 隐私红线，默认关）
}

// config 合并三层配置。布尔开关只单向开（false 不覆盖）——面板逐项提交时
// 未勾选的字段会以 false 出现，若允许覆盖会把手写配置里的 true 打回 false。
//
// 特例：base_url 显式空串视为「清空」。旧版装置（temp/setup_cap_e2e.py）靠传空串
// 让渠道打真实上游，若空串也当「未设置」，就会静默回落到 <data>/config.json 里的
// mock 地址，live 测试变假阳性（W3-36 陷阱 44 同源）。
func (ch *Channel) config(raw json.RawMessage) Config {
	cfg := Config{BaseURL: ch.Base, Timeout: ch.Timeout, Auth: ch.Auth, Proxy: ch.Proxy}
	apply := func(m map[string]any) {
		if m == nil {
			return
		}
		if s, ok := m["base_url"].(string); ok {
			cfg.BaseURL = s // 空串 = 显式清空，回落官方地址
		}
		if s, _ := m["auth"].(string); s != "" {
			cfg.Auth = s
		}
		if s, _ := m["proxy"].(string); s != "" {
			cfg.Proxy = s
		}
		if v, ok := m["allow_image_host"].(bool); ok && v {
			cfg.AllowImageHost = true
		}
		switch v := m["timeout"].(type) {
		case float64: // JSON 数字
			if v > 0 {
				cfg.Timeout = time.Duration(v) * time.Second
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.Timeout = time.Duration(n) * time.Second
			}
		}
	}
	apply(ch.cfg)
	if len(raw) > 0 {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			apply(m)
		}
	}
	return cfg
}

// saveConfig 把合并后的键写回 <data>/config.json（原子写 0600）。
// 只写可落盘的普通配置项——凭据（refresh_token）绝不进配置文件。
func (ch *Channel) saveConfig() error {
	if err := os.MkdirAll(ch.Data, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ch.cfg, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(ch.Data, "config.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// setCfg 合并一个键值进内存配置（空串/空值视为「未设置」，不覆盖）。
func (ch *Channel) setCfg(k string, v any) {
	if ch.cfg == nil {
		ch.cfg = map[string]any{}
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case nil:
		return
	}
	ch.cfg[k] = v
}

// Record 写一条流水（stat 未初始化时静默跳过）。token 未上报记 -1 由调用方保证。
func (ch *Channel) Record(rec stat.Record) {
	if ch.stat == nil {
		return
	}
	rec.Channel = ID
	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}
	ch.stat.Append(rec)
}

// Logins 暴露登录编排（CLI login 命令直连用；IPC 走 auth.* 方法）。
func (ch *Channel) Logins() *glm.LoginManager { return ch.logins }

// Store 暴露凭据库（CLI accounts/login 直连用）。
func (ch *Channel) Store() *glm.W3Store { return ch.store }
