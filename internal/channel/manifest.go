package channel

// manifest 与方法表组装。

import (
	"context"

	"wild-work3/chatglm-cli/internal/protocol/jsonl"
	"wild-work3/chatglm-cli/internal/protocol/meta"
	"wild-work3/chatglm-cli/internal/protocol/plugin"
)

// Manifest 返回 ww3 渠道插件清单（PLUGIN_CLI §4）。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID: ID, Name: Name, Version: Version, Kind: "channel",
		Protocol:    meta.ProtocolRng{Min: 1, Max: 1},
		Execution:   meta.Execution{Control: "call", Conversation: "call", MaxWorkers: 4},
		Icon:        "glm",
		Color:       "#3b6ef6",
		Description: "智谱清言 chatglm.cn 逆向渠道（IDE 指纹通道；支持主对话/深度思考/沉思/AI搜索/PPT/视频 与文生图/参考图生图）",
		// 安装后自动建一个渠道实例（W3-43：插件声明默认渠道，面板不再要求手工添加）
		DefaultChannels: []meta.DefaultChannel{{ID: ID, Name: Name, Config: map[string]any{}}},
		Capabilities: meta.Capabilities{
			// images.generate / systemone.decide 归控制面（PLUGIN_PROTOCOL §5.8）
			Control: []string{"models.list", "config.schema", "config.get", "config.set",
				"accounts.list", "accounts.remove", "accounts.refresh", "accounts.checkin",
				"accounts.resource_detail", "credits.get",
				"auth.methods", "auth.begin", "auth.status", "auth.finish", "auth.cancel",
				"images.generate", "systemone.decide", "stats.get",
				// 由 reg.Lifecycle 自动注册（glm-ext 也声明了这两项，保持能力表一致）
				"health.check", "ping"},
			Conversation: []string{"chat.generate"},
		},
	}
}

// Registry 组装全部方法（含 Lifecycle 钦定的 initialize/ping/health.check 等）。
func (ch *Channel) Registry() *plugin.Registry {
	reg := plugin.New()
	reg.Lifecycle(Manifest())

	// ── 会话面 ──
	reg.HandleStream("chat.generate", ch.ChatGenerate)

	// ── 控制面 ──
	reg.Handle("images.generate", ch.ImagesGenerate) // 无事件帧，只写一帧 result（W3-20）
	reg.Handle("models.list", ch.ModelsList)
	reg.Handle("config.schema", ch.ConfigSchema)
	reg.Handle("config.get", ch.ConfigGet)
	reg.Handle("config.set", ch.ConfigSet)
	reg.Handle("accounts.list", ch.AccountsList)
	reg.Handle("accounts.remove", ch.AccountsRemove)
	// 账号生命周期（合并自 glm-ext：保活续期 / 签到 / 额度明细 / 积分余额）
	reg.Handle("accounts.refresh", ch.AccountRefresh)
	reg.Handle("accounts.checkin", ch.AccountCheckin)
	reg.Handle("accounts.resource_detail", ch.ResourceDetail)
	reg.Handle("credits.get", ch.CreditsGet)
	reg.Handle("auth.methods", ch.AuthMethods)
	reg.Handle("auth.begin", ch.AuthBegin)
	reg.Handle("auth.status", ch.AuthStatus)
	reg.Handle("auth.finish", ch.AuthFinish)
	reg.Handle("auth.cancel", ch.AuthCancel)
	reg.Handle("stats.get", ch.StatsGet)
	// systemone.decide：契约钦定「命令仍须存在」——注册兜底句柄回 not_supported，
	// core 不会把本插件选进 systemone（manifest 未声明）。
	reg.Handle("systemone.decide", func(ctx context.Context, params jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return nil, errf("not_supported", "channel", "glm 渠道不支持 systemone.decide（未接入系统一决策）")
	})
	return reg
}
