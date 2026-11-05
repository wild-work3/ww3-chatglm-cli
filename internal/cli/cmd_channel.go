// cmd_channel.go —— ww3 PLUGIN_CLI 契约的四个必需命令 + 可选能力命令。
//
// 这些命令是 core 的调用面，行为逐条对齐 docs/PLUGIN_CLI.md §2：
//   - call  ：stdin 读一帧 invoke → 事件流 + 一帧 result → 退出（一请求一进程，W3-20）；
//   - stdio ：JSONL RPC 循环，进程长驻（与 call 共用同一个 registry）；
//   - manifest ：打印 manifest.json（--out 落文件）；
//   - ping  ：自检，逐方法打 ✓/✗ 到 stderr；
//   - systemone ：glm 渠道不支持 systemone.decide，命令仍须存在（§2.1.2 姿势）。
//
// call/stdio 只接受 --data（§3）：业务参数全部从 params 来，渠道配置走
// params.channel_config / <data>/config.json。
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"wild-work3/chatglm-cli/internal/channel"
	pcli "wild-work3/chatglm-cli/internal/protocol/cli"
)

// mainCtx 是 Main 构造的信号感知 context（注册先于 env 构造，用包级变量传递）。
var mainCtx context.Context

func registerChannel(app *pcli.App, reg map[string]handler) {
	{
		c := app.Cmd("call", "单次 IPC 调用（ww3 core 入口：stdin 一帧 invoke → 事件+result）")
		data := c.Str("data", "", "数据目录（缺省 $WW3_DATA → exe 所在目录）")
		reg["call"] = func(*pcli.Command) int {
			ch, err := channel.New(pcli.ResolveData(*data))
			if err != nil {
				fmt.Fprintf(os.Stderr, "打开数据目录失败: %v\n", err)
				return 1
			}
			if err := ch.Registry().ServeOnce(mainCtx, os.Stdin, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "call: %v\n", err)
				return 1
			}
			return 0
		}
	}
	{
		c := app.Cmd("stdio", "IPC 长驻循环（ww3 core 可选形态）")
		data := c.Str("data", "", "数据目录（缺省 $WW3_DATA → exe 所在目录）")
		reg["stdio"] = func(*pcli.Command) int {
			ch, err := channel.New(pcli.ResolveData(*data))
			if err != nil {
				fmt.Fprintf(os.Stderr, "打开数据目录失败: %v\n", err)
				return 1
			}
			if err := ch.Registry().ServeStdio(mainCtx, os.Stdin, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "stdio: %v\n", err)
				return 1
			}
			return 0
		}

	}
	{
		c := app.Cmd("manifest", "打印插件 manifest.json（--out 落文件）")
		c.Str("data", "", "数据目录（不影响输出，仅对齐通用开关）")
		out := c.Str("out", "", "写入文件而非 stdout")
		reg["manifest"] = func(*pcli.Command) int {
			b, err := json.MarshalIndent(channel.Manifest(), "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "marshal manifest: %v\n", err)
				return 1
			}
			b = append(b, '\n')
			if p := *out; p != "" {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "建目录失败: %v\n", err)
					return 1
				}
				if err := os.WriteFile(p, b, 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "写文件失败: %v\n", err)
					return 1
				}
				fmt.Fprintf(os.Stderr, "manifest 已写入 %s\n", p)
				return 0
			}
			os.Stdout.Write(b)
			return 0
		}

	}
	{
		c := app.Cmd("ping", "自检：枚举 registry 全部方法，逐个打 ✓ 到 stderr")
		data := c.Str("data", "", "数据目录（缺省 $WW3_DATA → exe 所在目录）")
		reg["ping"] = func(*pcli.Command) int {
			ch, err := channel.New(pcli.ResolveData(*data))
			if err != nil {
				fmt.Fprintf(os.Stderr, "✗ 打开数据目录: %v\n", err)
				return 1
			}
			ms := ch.Registry().Methods()
			sort.Strings(ms)
			for _, m := range ms {
				fmt.Fprintf(os.Stderr, "✓ %s\n", m)
			}
			return 0
		}
	}
	{
		c := app.Cmd("systemone", "（本渠道不支持 systemone.decide：存在但恒报 not_supported）")
		c.Str("data", "", "数据目录（仅对齐通用开关）")
		reg["systemone"] = func(*pcli.Command) int {
			fmt.Fprintln(os.Stderr, "not_supported: glm 渠道未实现 systemone.decide")
			return 2
		}

	}
}
