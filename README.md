# chatglm-cli（命令名 `glm2cli`）—— 智谱清言私有接口的 Go 命令行客户端

模块路径：`wild-work3/chatglm-cli`，入口在 `cmd/glm2cli`（所以 `go install` 产出的二进制就叫 `glm2cli`）。

单个静态二进制（零第三方依赖），把 [chatglm.cn](https://chatglm.cn) 网页/IDE 私有接口暴露成命令行：
**输入提示词 → 持续输出 JSONL 事件流**，多轮对话靠显式传 `--cid`（conversation_id）续接。
人和 agent 用同一套契约：flag 可写在任意位置、stdout 只放数据、日志走 stderr、永不交互。

> 协议依据：[`docs/PROTOCOL.md`](https://github.com/pyichiban/chatglm2api/blob/main/docs/PROTOCOL.md)、
> [`docs/生图协议与实操.md`](https://github.com/pyichiban/chatglm2api/blob/main/docs/生图协议与实操.md)（在兄弟仓）。
> 本 CLI 的设计与实测结论：[`docs/glm2cli设计.md`](docs/glm2cli设计.md)。
> Python 侧同协议实现（共用同一份凭据文件格式）：[`pyichiban/chatglm2api`](https://github.com/pyichiban/chatglm2api)。

## 1. 构建与安装

> 依赖：仅 Go 标准库 + `golang.org/x/image`（官方扩展包，只用 webp/bmp/tiff 解码与 CatmullRom
> 缩放、basicfont 标注，用于参考图拼版）；协议层与 CLI 本体零第三方依赖。

```bash
go test ./...                                   # 离线测试（真实帧回放，不发网络请求）
bash build.sh                                   # win+linux 单文件到 dist/（产物 dist/chatglm-cli.exe）

# 手动构建
go build -trimpath -ldflags "-s -w" -o dist/chatglm-cli.exe ./cmd/glm2cli     # Windows
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/chatglm-cli ./cmd/glm2cli

# 装成全局命令（产出可执行文件 glm2cli）
go install wild-work3/chatglm-cli/cmd/glm2cli@latest
```

> **构建产物名**：`bash build.sh` 产 `dist/chatglm-cli.exe`（ww3 交付物约定名）。
> `go install ./cmd/glm2cli` 仍按 Go 规则产 `glm2cli`，两者是同一个程序的两种命名。
> `scripts/build.sh` 保留为兼容垫片，直接转调仓根 `build.sh`。

拷到 PATH 即可（例如 `D:/dev/Git/cmd/glm2cli.exe`，与 rg/fd/jq 同目录）。

## 2. 取凭据（唯一的入参入口）

清言没有可编程登录接口，凭据只能由用户从浏览器取出：

1. 浏览器登录 <https://chatglm.cn>，`F12` → `Application` → `Storage` → `Cookies` → `https://chatglm.cn`；
2. 复制 **`chatglm_refresh_token`** 的值（它才是根凭据，access token 自动换取）；
3. 写进凭据文件（推荐，**refresh_token 每次刷新都可能轮换，只有文件模式能落盘**）：

```bash
cp glm_credential.example.json glm_credential.json   # 仓库根；填入 refresh_token
```

凭据查找顺序：`--cred PATH` > `$GLM_CREDENTIAL_FILE` > `./glm_credential.json` > `~/.glm2cli/credential.json`
> 全部缺失时回落环境变量 `GLM_REFRESH_TOKEN` / `GLM_ACCESS_TOKEN` / `GLM_DEVICE_ID` / `GLM_USER_ID` / `GLM_COOKIE`
（env 模式无法落盘 ⇒ 刷新后新根凭据只活在本次进程里，会打印 warn 事件）。

文件支持三种写法：JSON 对象、整条 Cookie 串、裸 refresh token 一行。JSON 里**本工具不认识的键会被原样保留**
（与 Python 侧共用同一个 `glm_credential.json` 不会互相丢字段）。

## 3. 命令一览

```
glm2cli chat   [flag] [提示词]      流式对话（唯一的事件流命令）
glm2cli user                       当前用户信息（含积分余额）
glm2cli models                     可用模型 + reasoning_effort 枚举
glm2cli agents                     内置智能体（assistant_id）表
glm2cli auth [check|refresh]       凭据自检 / 强制刷新并落盘
glm2cli conv <子命令>               会话增删改查（见下）
glm2cli raw                        逃生门：直打任意 /chatglm/ 端点（补实证用）
glm2cli schema                     机器可读契约（命令/事件/退出码）
glm2cli help [conv|version]        用法
```

会话子命令：

```
conv list                [--agent ID|别名] [--grep 关键词]     全部智能体的完整列表（内部自动翻页，默认不过滤）
image                    一键生图（阻塞）：见 §3.5
conv show       CID                                 元数据 + 自动标题
conv history    CID      [--agent A] [--raw]        逐条输出 user/text/reasoning/images
conv rename     CID TITLE|--title T|--title-file P   改标题
conv title      CID                                 只读自动标题
conv delete     CID...   --yes [--agent drawing]    批量删（agent 必须匹配，否则上游静默无效）
conv stop       MESSAGE_ID                          中止生成
conv read       MESSAGE_ID                          标记已读
```

### chat 的 flag

| flag | 说明 |
|---|---|
| `-p, --prompt` | 提示词。`-` 强制从 stdin 读。缺省时若有位置参数用位置参数，否则自动读 stdin |
| `--cid` | **conversation_id（24 位小写 hex）**。省略 = 新会话（服务端懒创建，ID 从首帧回传） |
| `--agent` | `main`(默认) \| `drawing` \| `read` \| `video` \| `study` \| 裸 assistant_id |
| `--model` | `selected_model`，默认 `glm-5.3-flash`（`models` 可查枚举） |
| `--mode` / `--effort` | 原样透传 `chat_mode` / `reasoning_effort`（映射见「已知缺口」） |
| `--web=false` | 关闭联网检索（默认开） |
| `--ratio` | 生图比例 `1:1\|3:4\|4:3\|9:16\|16:9`；给了它就自动切 `drawing` agent |
| `--title` | 新会话首轮答完顺手改名（「增 + 改」一次做完） |
| `--style` / `--scene` | 生图风格/场景，**实测已失效**，仅透传 |

全局 flag：`--cred --base --timeout --proxy --dry-run -v --insecure`；`--version`/`-V` 可放任意位置。`--proxy` 未给时读环境变量 `HTTPS_PROXY`/`HTTP_PROXY`（国内直连更快，代理仅供海外环境用）。
`--timeout` **只作用于非流式请求**（含凭据刷新）；对话流刻意不设任何超时（带超时会把长回答掐断且没有终止帧）。

`raw` 的例子（本次就是靠它发现历史端点的，不需要为探测改代码重编）：

```bash
glm2cli raw --path chatglm/mainchat-api/conversation/messages \
  --param conversation_id=6ad171b95c88cc6f53bb2a74 --param assistant_id=65940acff94777010aa6b796
```

> git-bash 会把 `/` 开头的参数换码 ⇒ `--path` 请写 `chatglm/…`（不写前导斜杠），或 `MSYS_NO_PATHCONV=1`。

## 4. 用法示例

**人**

```bash
glm2cli chat "用一句话介绍你自己"                       # 直接敲，事件流刷出来
printf '很长的多行提示词\n第二行' | glm2cli chat        # stdin
glm2cli chat --cid 6ad16e02581cc70756919ade "继续"     # 续接上一轮
glm2cli chat --ratio 3:4 --title 猫 "戴礼帽的橘猫"      # 文生图（画图 agent）+ 顺手改名
glm2cli user | jq .data.left_score                     # 余额（单位「分」，原样不折算）
glm2cli conv list | jq -r 'select(.type=="item").data.title'   # 完整列表（内部自动翻页）
```

### 3.5 `image` 一键生图（阻塞直到完成或出错）

```bash
# 文生图（无参考图，走画图 agent，一次最多 4 图，自动下载）：
glm2cli image "一只戴黑色礼帽的橘猫，扁平插画" --out ./out

# 参考图生图（1~N 张：自动拼版 → 图床上传 → 智能编辑，产物单图）：
glm2cli image --out out.jpg \
  "reference1 是鲸鱼娘角色，reference2 是精灵角色；让两人同框交谈，身份特征严格遵循参考图" \
  whale.jpg vesna.jpg
```

内部流程（每步都有 item 进度事件）：

1. **拼版**（仅当有参考图）：**单行左右布局**（N 不限）—— 所有参考图等高、从左到右排列，
   单元图高二分搜索最大化（目标 ≤480w，上游 500w 硬约束留余量；单图放大上限 4 倍）；
   每张参考图外围画**方形外框**（画在图像外侧，不遮像素），**标注条在框内、图像正下方**
   （`reference1..N`，归属无歧义），框间留白 40px；
   提示词前自动注入硬编码约束「不要复刻参考图的排版样式」（见 contract.json prompt_prefix）；
2. **上传图床**：file.sang.pub `meitu` 通道（落地 meitudata.com 大陆 CDN），失败自动回退
   `icve` 通道；raw binary body（multipart 会被存成信封，勿改）；
3. **智能编辑**：`POST drawing-api/v1/image/reference`（17~22s 同步；单图接口；输出比例跟随输入图；
   **`rm_label_watermark` 写死 true** —— 实证 false 产物带「清言·AI生成」角标，永不产带水印图）；
4. **下载保存**：`--out` 为目录或 `.jpg` 文件路径，默认当前目录按时间戳命名；非 JPEG/PNG 直接报错。

> 提示词里用 `reference1`/`reference2` 指代对应参考图；**要明确“只画哪张/画哪几张”**，
> 否则模型可能照搬拼版的左右布局。两个服务都是免费匿名第三方，参考图勿含敏感内容。

**agent（无 TTY、只靠契约与退出码）**

```bash
CID=$(glm2cli chat --prompt-file q.txt | jq -r 'select(.type=="meta").conversation_id')
glm2cli chat --cid "$CID" -p "我上一个问题是什么" >> trace.jsonl
glm2cli conv rename "$CID" "已处理" && glm2cli conv delete "$CID" --yes
```

管道里逐行渲染正文：

```bash
glm2cli chat -p "数到 5" | jq -rR 'fromjson|select(.type=="text").delta' | tr -d '\n'
```

**先把请求看一遍再打**（不发网络）：

```bash
glm2cli chat --dry-run -p hi
glm2cli conv delete 6ad16e02581cc70756919ade --yes --dry-run
```

## 5. 输出契约（JSONL）

一行一个事件，UTF-8，`\n` 结尾，无 ANSI。`v:1` 是模式版本，未知字段忽略。

| type | 出现时机 | 关键字段 |
|---|---|---|
| `meta` | 对话流第一个事件（拿到首帧才发，故 `conversation_id` 必是真实值） | `conversation_id` `message_id` `agent_id` `model` `new` `request_id` `ts` |
| `text` / `reasoning` | 增量 | `part`（段落 logic_id）`delta` |
| `image` | 生图产物 | `url` `polished_prompt`（上游润色后的实际 prompt）`index` |
| `item` | 列表/历史逐条 | `cmd` `data` |
| `result` | 单发命令的载荷 | `cmd` `data` |
| `done` | 收尾统计 | `conversation_id` `message_id` `frames` `elapsed_ms` `finished` `images` `text_chars` `reasoning_chars` `count` |
| `warn` | 非致命 | `code` `message` |
| `error` | 失败 | `code` `message` `detail` `retryable` `http_status` `biz_status` |

要点（都是实证换来的）：

- **把 `text` 的 `delta` 依次拼接 == 上游权威全文**：`init` 帧是增量、`finish` 帧是该段全文，
  客户端按前缀差分只补发「还没发出去的后缀」，重复推送会被 dedup（短回答常常**只有 finish 帧**，
  没有 init 增量 —— 只发 init 会把正文整个丢掉）。前缀对不上时改发 `warn/delta_resync` 并带全文，不静默丢内容。
- 上游**不发** `data:[DONE]`；正常收尾 = 顶层 `status:"finish"`（见 `done.finished`）。
- 生图流末尾有一行**不带 `data:` 前缀**的裸 JSON 信封 `{"status":0,…,"rid":…}`，解析器按「status 不是字符串」忽略它。
- 上游整条流**没有任何 token/计费字段**，故无 `usage`；不要指望从这里拿用量。
- `--dry-run` 也走同一套事件（`result cmd=dry-run`），便于 agent 先看请求再决定是否执行。

### 退出码

| code | code 名 | 含义 | 可重试 |
|---|---|---|---|
| 0 | ok | 成功 | — |
| 1 | upstream_error | 上游业务/HTTP 错误、响应非 JSON（WAF 拦截页） | 视情况 |
| 2 | usage_error | flag/参数非法（含缺 `--yes`、`--cid` 形状错、`--ratio` 非法） | 否 |
| 3 | auth_error | 凭据失效（401/40102/40103、refresh 被拒、访客账号） | 否，需重新取凭据 |
| 4 | transient_rejection | 风控瞬时拒绝 40012 | 是（稍后手动重跑） |
| 5 | stream_truncated | 流被截断/被 Ctrl-C ⇒ 回答不完整 | 是 |
| 6 | rate_limited | HTTP 429 | 是（降速） |
| 7 | not_proven | 调了协议里未实证的接口 | 否 |

`glm2cli schema` 会把上面两张表 + 事件表以 JSON 输出（agent 可在运行时读契约）。

## 6. 行为边界（刻意的设计）

| 决定 | 理由 |
|---|---|
| **只用 IDE(Electron) 指纹通道** | WEB 通道的画图族接口要 `refer__991`（浏览器 JS 动态生成，无法离线复刻）；IDE 通道实测不需要 |
| **不做退避重试** | IDE 链路实测稳定；重试只会掩盖问题。真遇到 40012 就 exit 4，让调用方决定重跑 |
| **只有流式，没有聚合模式** | 输入 → JSONL 持续输出是唯一形态。要全文就 `select(.type=="done")` 前把所有 delta 拼起来 |
| **必须显式传 `--cid`** | 不做 `@last`/别名/本地会话状态 ⇒ 多个会话并发时不可能互相污染 |
| **不落盘任何会话数据** | 唯一落盘的是凭据文件（refresh 轮换必须持久化）。历史要留就自己重定向：`glm2cli chat … >> conv.jsonl` |
| 流式请求不设任何超时 | 带超时会把长回答掐断且没有终止帧（上游不发 `[DONE]`） |
| 流被截断绝不伪造收尾 | 直接 `stream_truncated` + exit 5，并尽力调 `stop_stream` 中止服务端生成 |
| `messages` 只带当前一条 | 多轮上下文由服务端按 `conversation_id` 维护；客户端拼历史会与上游语义冲突 |
| Cookie 交给 jar，绝不手写 Cookie 头 | 显式头会覆盖 jar，使 WAF cookie（acw_tc/cdn_sec_tc）无法带回 ⇒ 持续 40012 |
| 不做 image/edit 命令 | 生图就是「对话通道换 agent + cogview 参数」⇒ `chat --agent drawing --ratio` 已覆盖；参考图改图另说 |

## 7. 已知缺口 / 实测备注

| 项 | 状态 |
|---|---|
| 单会话历史 | ✅ **可用**：`GET mainchat-api/conversation/messages?conversation_id&assistant_id`（2026-10-16 实测，升序全量、无翻页）。老端点 `agent-api/conversation/page_messages_v2` 实测**始终空 list**，已弃用 |
| `--agent` 与会话必须匹配 | ⚠️ 会话 = (`assistant_id`, `conversation_id`) 二元组；`conv delete`/`conv history` 传错 agent 会**静默无效/返回空**，不报错 |
| `--mode` / `--effort` 映射 | ⚠️ 两套枚举并存：`models` 给 `effort`=`""/thinking/deep_thinking` 与 `am_effort`=`low/high/max`；抓包请求体是 `chat_mode:"thinking"` + `reasoning_effort:"high"`。故本 CLI 一律**原样透传**，不猜映射 |
| 删除产物图片 | 删会话**不回收**生图产物，直链仍可用（靠 history_id 不可枚举保密） |
| 图链有效期 | 上游未给过期时间 ⇒ 需要长期保留就立刻下载（`image.url` 加 `?image_process=format,png` 可转真 PNG） |
| 参考图改图 / 上传 | ❌ 本版未实现（`chatglm/drawing-api/v1/image/reference` 只收上游可达的公网 URL；`chat_upload` 被 WAF 拦） |
| 工具调用 / 登录逆向 / 多账号 | ❌ 明确不做（与 Python 侧同边界） |

## 8. 故障排查

| 现象 | 处理 |
|---|---|
| `auth_error`（3） | refresh_token 已失效或在别的设备登出 ⇒ 重新从浏览器取 Cookie 写回凭据文件 |
| `transient_rejection`（4） | 被 WAF 瞬时拒绝 ⇒ 降低频率、稍后重跑；别加 `--v` 猛试 |
| `stream_truncated`（5） | 网络/代理掐断 ⇒ 同 `--cid` 重问一次即可（服务端可能已生成，先 `conv history` 核对） |
| 控制台中文乱码 | 二进制启动时已自动 `SetConsoleOutputCP(65001)`；重定向到文件永远是 UTF-8 |
| git-bash 下 `raw --path /chatglm/x` 报路径错 | MSYS 会把 `/` 开头的参数换码，改写成 `--path chatglm/x`（或 `MSYS_NO_PATHCONV=1`） |
| 想看上游到底返回什么 | `glm2cli raw --path chatglm/… --param k=v`；或 `-v` 把日志打到 stderr |
