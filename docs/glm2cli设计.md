# glm2cli 设计与实现（ww3 渠道插件契约）

> glm2cli 是智谱清言（chatglm.cn 私有接口，IDE 指纹通道）的 **Go 单文件 CLI**。
> 自 v2.0.0 起按 ww3 项目 `protocol/docs/PLUGIN_CLI.md` 契约重构：**既是独立命令行工具，也直接是
> ww3 的渠道插件**（命令名 `glm2cli`，PluginID `glm`）。
> 上游协议事实源：`chatglm2api/docs/PROTOCOL.md`（[实证] 结论不重复，本文只写 CLI 形态）。

---

## 1. 定位与双重身份

| 身份 | 入口 | 说明 |
|---|---|---|
| ww3 渠道插件 | `glm2cli call --data <dir>` | core 每请求 spawn 一次进程，stdin 一帧 invoke → 事件流 + result（W3-20） |
| ww3 长驻形态 | `glm2cli stdio` | JSONL RPC 循环（可选，core 未用） |
| 人 / agent 直用 | `chat / images / conv-* / user / auth / login / ...` | 便利命令，glm2cli 自有 JSONL 事件契约 |

- **能力面 = chatglm2api（Python 库）超集 + ww3 IPC 归一化**：多账号 authstore、生图/参考图改图、
  会话管理全套、AI搜索/PPT 智能体、raw 逃生门、schema 自描述。
- 继承边界：不支持代理（V12）、无 refer__991、不做退避重试（40012 直接报 transient）、
  **不支持工具调用（chat.generate 收到 tools 直接忽略）**、**不支持多轮拼装（messages 只取最后一个
  user 提示词）** —— 后两条是用户拍板的有意裁剪。

## 2. 目录结构

```
cmd/glm2cli/main.go      入口（InitConsole + cli.Main）
internal/
  glm/        上游内核：cred/client/sse/api/history/sign/constants + ww3auth/ww3login（authstore 与登录编排）
  channel/    ww3 渠道层：manifest + IPC 方法表（chat.generate/images.generate/models.list/...）
  protocol/   ww3 main/protocol 底座副本（W3-28 惯例，只增不改；jsonl/rpc/plugin/meta/cli/stat/authflow/chat/errkind）
  cli/        命令壳：ww3 风格 FlagSet（双短横线全名、无位置参数）+ glm2cli 事件输出
  imgutil/    参考图拼版（单行左右布局+外框+标注）与图床上传
scripts/build.sh          CGO_ENABLED=0 单文件，win/linux 双平台
internal/{glm,channel}/testdata/contract.json   与 Python 仓逐字节一致的契约
```

## 3. ww3 契约命令（protocol/docs/PLUGIN_CLI.md §2）

| 命令 | 行为 |
|---|---|
| `call` | stdin 读一帧 invoke → 归一事件流 + 一帧 result → 退出。**只接受 `--data`** |
| `stdio` | 同 registry 的 JSONL RPC 循环 |
| `manifest` | 打印 manifest.json（`--out` 落文件） |
| `ping` | 枚举 registry 全部方法，逐个 ✓ 打 stderr，退出 0 |
| `systemone` | glm 渠道不支持 systemone.decide：命令存在、stderr 打 `not_supported`、退出 2 |
| `images` | 生图便利命令：**spawn 自身 call 走真实 IPC**（§4 纪律），见 §5 |

### 3.1 manifest

```json
{"id":"glm","name":"智谱清言","version":"2.0.0","kind":"channel","protocol":{"min":1,"max":1},
 "execution":{"control":"call","conversation":"call","max_workers":4},
 "capabilities":{"control":["models.list","config.schema","accounts.list","accounts.remove",
   "auth.methods","auth.begin","auth.status","auth.finish","auth.cancel","images.generate",
   "systemone.decide","stats.get"],"conversation":["chat.generate"]}}
```

### 3.2 IPC 方法要点

- **chat.generate**（唯一 StreamHandler）：事件序列 `text.delta{content}` → `finish_reason{reason}` →
  `usage{time_ms,first_token_ms}` → `completed{}`，终止只一次；result = usage 对象。
  glm 上游没有 token 计数 ⇒ 不发 prompt/completion_tokens（契约：null=未提供≠0），stat 流水记 -1。
  模型命名 `glm/<model>`：`chatglm:moe_53f`（主对话）/`chatglm-think:moe_53f`（zero 模式）/
  `chatglm-deepresearch:moe_53f`（deep_research 模式）/`chatglm-search`/`chatglm-ppt`/`chatglm-video`/
  24 位 hex 直接当 assistant_id；未知模型名透传 selected_model，非 `glm/` 前缀报 model_blocked。
  `reasoning_effort` 透传；`thinking:true` 映射 `effort=thinking`；`conversation_id` 24hex 校验；
  `tools/tool_choice/temperature` 等一律忽略。
  错误映射：40012→soft_rate/account(retryable)、401→auth/account、截断→truncated/channel、其余→server/channel。
- **images.generate**（控制面 plain Handler，无事件帧）：params `{prompt,n(1-4),size,aspect_ratio,
  response_format(b64_json|url,缺省 b64_json),images[]{b64_json,mime_type}|{url},uid,config}`；
  ratio 与 size 同给时 **ratio 优先**；ratio 校验集合 `1:1|3:4|4:3|9:16|16:9`（size WxH 约分后必须落进集合）。
  0 张参考图 → cogview 文生图（n>1 串行、间隔 2s 风控纪律）；≥1 张 → 客户端拼版（imgutil）→
  图床直链 → `drawing-api/v1/image/reference`（单图、输出比例跟随输入图、去水印写死 true）。
  result `{created,data:[{url|b64_json,revised_prompt,mime_type,width,height,index}],aspect_ratio,note,usage:null}`。
  参考图字节合计 ≤24MB；本地路径绝不进 IPC params。
- **models.list**：静态表（上游无公开模型接口），`default: glm/chatglm:moe_53f`。
- **systemone.decide**：注册但恒回 `not_supported`（scope channel；Rotatable=false）。
- **auth.\***：form 范式（粘贴 refresh_token）。begin 同步返回 `waiting_input`；
  finish 验活（强制刷新）→ uid/nickname 落 authstore → success（token 绝不回传）。
- **accounts.list/remove**：离线操作 auths 目录；RefreshToken 空标 session_dead。
- **stats.get**：`<data>/stat/stats.jsonl` 的 Query/Summarize/DailyRows。

### 3.3 数据目录（PLUGIN_CLI §5/§6）

`<data>/{auths,logs,stat}`；`--data` 缺省 `$WW3_DATA` → exe 所在目录（绝不退 cwd）。
每条 stat 流水 fsync；token 未上报记 **-1**（cached_tokens 同）。

## 4. 凭据体系（internal/glm/ww3auth.go）

- authstore 嵌套形：`{"auth":{accessToken,refreshToken,expiresAt,...},"account":{uid,nickname}}`，
  文件名 = 凭据标识 = core 侧 `account_uid`（如 `glm-6982e0ad0efeac7998789a48`）。
- 读兼容：嵌套形 / glm2cli 旧 JSON / 整条 cookie / 裸 token。
- **轮换落盘**：refresh 会轮换 refresh_token，必须写回（tmp+rename 0600）。写前乐观比对磁盘
  refresh_token 与本次刷新依据，不一致 ⇒ 判并发轮换，放弃覆盖。
- **未知键保留**：解析时抓 `unknown`（如 machineId/apiHost/baizhiCookie 等部署侧字段），
  轮换重写时原样带回，绝不丢字段（有回归测试）。
- 便利命令回落链：`--auth` → auths 里第一个未过期 → 旧版 glm_credential.json / GLM_* 环境变量。
- 渠道配置 `params.channel_config` > `<data>/config.json`，键：`auth/base_url/timeout`（不支持 proxy）。

## 5. 便利命令（glm 私有，core 不依赖）

```
chat    --prompt(- / stdin) --cid --agent main|drawing|read|video|study|search|ppt|<24hex>
        --model --mode --effort --web --ratio 1:1|3:4|4:3|9:16|16:9 --style --scene --title
        输出 glm2cli JSONL 事件流：meta(首帧后先发,含真实 conversation_id) → text/reasoning/image
        → done{frames,elapsed_ms,finished,images,text_chars,reasoning_chars}；截断/Ctrl-C 尽力 stop_stream
images  --prompt --n 1-4 --size WxH|auto --ratio w:h --ref url|路径|dataURI(可重复) --format b64_json|url
        --out-dir(缺省 <data>/images) --no-save --raw；spawn 自身 call 走 IPC，默认落盘打路径清单
conv-list/show/history/rename/delete/stop/read   （--cid/--message-id 均 24hex；delete 需 --yes）
user / models(--list 静态表|缺省透传上游) / agents / accounts(--remove --auth N)
auth    --refresh；view{source,user_id,device_id,access_exp,refresh_exp,access_expired,can_persist}
login   form 粘贴 refresh_token：--token 或 stdin 一行；--status/--cancel --flow <id> 续查
raw     --method GET|POST --path --body(-=stdin) --param k=v     （逃生门，调试用）
schema / version / help
```

输出契约（contract.json `cli_event_contract`）：stdout 只出 JSONL（meta/text/reasoning/image/item/
result/done/warn/error），stderr 只出日志；退出码 = glm.Error.Code：0 ok / 1 upstream / 2 usage /
3 auth / 4 transient(40012) / 5 truncated / 6 rate_limited / 7 not_proven。
`chat` 的账号解析走 §4 回落链；IPC 路径则严格 `account_uid`（会话面）/`uid`（控制面）。

## 6. 纪律与实测结论（本仓新增）

- 风控纪律：一次只改一个变量、请求间隔 ≥5s、不密集重试；images n>1 内置 2s 间隔。
- 参考图/凭据的 base64 绝不落日志（W3-07），日志只记张数/字节数/mime。
- 便利命令 `images` 与 IPC 走**同一份**生成代码（channel.ImagesGenerate），行为不会分叉。
- 实机验证（凭据 glm-6982e0ad0efeac7998789a48，真实 ww3 部署目录）：auth 刷新+轮换落盘 ✓、
  user ✓、chat.generate 全事件序列 ✓、images.generate b64_json 1024×1024 ✓、conv-list ✓、
  models.list/systemone/not_found/stats.get ✓。
- 已知限制：conv 历史依赖 `mainchat-api/conversation/messages`（assistant_id 不匹配返回空列表）；
  `conv delete` 的 agent_id 不匹配时上游静默无效；webp/gif 尺寸不解析（无第三方解码器）。

## 7. 作为「web 类逆向渠道 CLI 标准契约」的移植指引

chatgpt-cli 等同类工具照此结构移植：`internal/protocol`（原样复制）+ `internal/<chan>`（内核）+
`internal/channel`（IPC 归一层）+ `internal/cli`（ww3 风格壳）。必须对齐的硬点：
call/stdio 只认 `--data`；manifest/ping/systemone/images 命令形制；authstore 嵌套形+轮换落盘+
未知键保留；chat.generate 事件序列与 usage 归一；stderr/stdout 分离；退出码表。
教训：注册命令时闭包捕获的 flag 指针必须每命令独立作用域（`cf`/`data` 复用变量会让所有命令
读到最后一个命令的 flagset）——三个仓都踩过。
