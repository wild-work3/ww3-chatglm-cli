// register.go —— 命令注册总表（Main 一次性装配）。
package cli

import pcli "wild-work3/chatglm-cli/internal/protocol/cli"

func registerAll(app *pcli.App) map[string]handler {
	reg := map[string]handler{}
	registerChannel(app, reg)
	registerChat(app, reg)
	registerConv(app, reg)
	registerInfo(app, reg)
	registerRaw(app, reg)
	return reg
}
