// glm2cli —— 智谱清言（chatglm.cn）私有接口的 Go 命令行客户端。
//
// 契约：stdout 每行一个 JSON 事件，日志走 stderr，永不交互；详见 `glm2cli schema` 与 README.md。
package main

import (
	"os"

	"wild-work3/chatglm-cli/internal/cli"
)

func main() {
	cli.InitConsole()
	os.Exit(cli.Main(os.Args[1:]))
}
