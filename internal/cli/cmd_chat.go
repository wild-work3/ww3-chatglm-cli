// cmd_chat.go —— 对话与生图的便利命令（glm2cli 自有事件契约）。
//
// chat   ：直连内核流式（glm2cli JSONL 事件流，contract.json cli_event_contract）；
// images ：生图，内部 spawn 自身 call 走真实 IPC（PLUGIN_CLI §4：自定义命令
//
//	一律复用 call 契约路径），产物按 --format 落盘或透传。
package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	pcli "wild-work3/chatglm-cli/internal/protocol/cli"
	"wild-work3/chatglm-cli/internal/protocol/jsonl"
	"wild-work3/chatglm-cli/internal/protocol/plugin"
)

func registerChat(app *pcli.App, reg map[string]handler) {
	// 闭包必须捕获本命令自己的 flagset 指针 ⇒ 每个命令用独立块作用域
	{
		c := app.Cmd("chat", "对话（流式；--agent drawing/--ratio 走生图）")
		prompt := c.Str("prompt", "", "提示词；\"-\" 或缺省时读 stdin")
		cid := c.Str("cid", "", "续聊会话 ID（24 位 hex，懒创建时首帧回传新 ID）")
		agent := c.Str("agent", "", "智能体：main|drawing|read|video|study|search|ppt 或 24 位 hex")
		model := c.Str("model", "", "模型名（selected_model 透传上游）")
		mode := c.Str("mode", "", "chat_mode（普通/思考/沉思等，透传上游）")
		effort := c.Str("effort", "", "思考档位（thinking/deep_thinking/low/high/max）")
		web := c.Bool("web", true, "是否联网（默认开）")
		ratio := c.Str("ratio", "", "画幅 1:1|3:4|4:3|9:16|16:9（给出即走画图智能体）")
		style := c.Str("style", "", "画面风格（透传 cogview.style）")
		scene := c.Str("scene", "", "场景（透传 cogview.scene）")
		title := c.Str("title", "", "生成完自动改会话标题")
		cf := addCommon(c)
		reg["chat"] = func(*pcli.Command) int {
			return runChat(*prompt, *cid, *agent, *model, *mode, *effort, web, *ratio, *style, *scene, *title, cf)
		}
	}
	{
		c := app.Cmd("images", "生图（文生图/参考图生图；spawn 自身 call 走 IPC 契约）")
		prompt := c.Str("prompt", "", "提示词；\"-\" 或缺省时读 stdin")
		n := c.Int("n", 1, "生成张数（1-4；多张串行，间隔 2s）")
		size := c.Str("size", "", "尺寸 WxH 或 auto（与 --ratio 同给时 ratio 优先）")
		ratio := c.Str("ratio", "", "画幅 w:h（校验集合同 chat --ratio）")
		refs := c.StrList("ref", "参考图：URL | 本地路径 | data URI，可重复")
		format := c.Str("format", "b64_json", "返回格式 b64_json|url（缺省 b64_json，直链会过期）")
		outDir := c.Str("out-dir", "", "落盘目录（缺省 <data>/images）")
		noSave := c.Bool("no-save", false, "只回 JSON 不落盘")
		raw := c.Bool("raw", false, "原样打印 result JSON（默认打落盘路径清单）")
		cf := addCommon(c)
		reg["images"] = func(*pcli.Command) int {
			return runImages(*prompt, n, *size, *ratio, refs, format, *outDir, *noSave, *raw, cf)
		}
	}
}

func runChat(prompt, cid, agent, model, mode, effort string, web *bool, ratio, style, scene, title string, cf *common) int {
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	p, err := readPrompt(prompt)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	aid, err := resolveAgent(agent, ratio)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if cid != "" && !glm.CIDPattern.MatchString(cid) {
		err := glm.NewErr(glm.CUsage, "--cid 必须是 24 位 hex")
		e.fail(err)
		return code(err)
	}
	if ratio != "" && !validRatio(ratio) {
		err := glm.NewErr(glm.CUsage, "--ratio 不支持 %q（1:1|3:4|4:3|9:16|16:9）", ratio)
		e.fail(err)
		return code(err)
	}
	ch, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	newConv := cid == ""
	opt := glm.ChatOpt{Agent: aid, CID: cid, Prompt: p, Model: model, Mode: mode,
		Effort: effort, Web: *web, Ratio: ratio, Style: style, Scene: scene}
	agg, err := e.streamChatEvents(c, opt, "chat", newConv)
	if err != nil { // 截断/中断：尽力停服务端，再报错（绝不伪造收尾）
		e.stopBestEffort(c, aggMsgID(agg))
		e.fail(err)
		ch.RecordChat(model, false, err, e.start)
		return code(err)
	}
	if agg.Err != nil {
		e.stopBestEffort(c, aggMsgID(agg))
		e.fail(streamErr(agg.Err))
		ch.RecordChat(model, false, streamErr(agg.Err), e.start)
		return code(streamErr(agg.Err))
	}
	e.ev(&glm.Event{Type: "done", Cmd: "chat", Frames: agg.Frames, ElapsedMS: time.Since(e.start).Milliseconds(),
		Finished: agg.Finished, Images: agg.Images, TextChars: len([]rune(agg.Text())), ReasoningChars: len([]rune(agg.Reasoning()))})
	if title != "" && agg.ConversationID != "" && glm.CIDPattern.MatchString(agg.ConversationID) {
		if _, err := c.Rename(agg.ConversationID, title); err != nil {
			e.ev(&glm.Event{Type: "warn", Message: "改标题失败: " + err.Error()})
		}
	}
	ch.RecordChat(model, true, nil, e.start)
	return 0
}

func aggMsgID(agg *glm.Agg) string {
	if agg == nil {
		return ""
	}
	return agg.MessageID
}

// streamErr 把内核聚合错误翻成事件错误（保持 contract.json 错误形状；
// 40012 → transient_rejection 退出码 4，其余 SSE 错误 → upstream_error）。
func streamErr(e *glm.ErrObj) *glm.Error {
	code := glm.CUpstream
	if e.ErrorCode == glm.STATUS_TRANSIENT {
		code = glm.CTransient
	}
	return &glm.Error{Code: code, Message: e.ErrMsg, BizStatus: e.ErrorCode}
}

// validRatio 画幅校验（与 cogview 实测支持集一致）。
func validRatio(r string) bool {
	switch r {
	case "1:1", "3:4", "4:3", "9:16", "16:9":
		return true
	}
	return false
}

// ── images（spawn 自身 call 走真实 IPC） ──

type imageEntry struct {
	URL           string `json:"url,omitempty"`
	B64           string `json:"b64_json,omitempty"`
	MimeType      string `json:"mime_type,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
	Index         int    `json:"index,omitempty"`
}

type imagesResult struct {
	Created     int64        `json:"created"`
	Data        []imageEntry `json:"data"`
	AspectRatio string       `json:"aspect_ratio,omitempty"`
	Note        string       `json:"note,omitempty"`
}

// extMime 仅认证据充分的扩展名，推不出报错（W3-07 不猜）。
func extMime(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", nil
	case ".jpg", ".jpeg":
		return "image/jpeg", nil
	case ".webp":
		return "image/webp", nil
	case ".gif":
		return "image/gif", nil
	case ".bmp":
		return "image/bmp", nil
	}
	return "", fmt.Errorf("无法从扩展名推断 MIME（%s）：只支持 png/jpg/jpeg/webp/gif/bmp", filepath.Base(path))
}

func runImages(prompt string, n *int, size, ratio string, refs *[]string, format *string, outDir string, noSave, raw bool, cf *common) int {
	dataDir := pcli.ResolveData(*cf.data)
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	p, err := readPromptVal(prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "glm2cli:", err)
		return 2
	}
	if *n < 1 || *n > 4 {
		fmt.Fprintln(os.Stderr, "glm2cli: --n 取值 1-4")
		return 2
	}
	// 参考图进 IPC params：本地文件读成 b64（绝不传本地路径，W3-07）
	imgs := make([]map[string]any, 0, len(*refs))
	for i, ref := range *refs {
		switch {
		case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
			imgs = append(imgs, map[string]any{"url": ref})
		case strings.HasPrefix(ref, "data:"):
			mime, payload, err := parseDataURI(ref)
			if err != nil {
				fmt.Fprintln(os.Stderr, "glm2cli:", err)
				return 2
			}
			imgs = append(imgs, map[string]any{"b64_json": payload, "mime_type": mime})
		default:
			mime, err := extMime(ref)
			if err != nil {
				fmt.Fprintln(os.Stderr, "glm2cli:", err)
				return 2
			}
			b, err := os.ReadFile(ref)
			if err != nil {
				fmt.Fprintln(os.Stderr, "glm2cli:", err)
				return 2
			}
			fmt.Fprintf(os.Stderr, "参考图 %d：%s（%d 字节，%s）\n", i+1, filepath.Base(ref), len(b), mime)
			imgs = append(imgs, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(b), "mime_type": mime})
		}
	}
	params := map[string]any{"prompt": p, "n": *n, "response_format": *format}
	if ratio != "" {
		params["aspect_ratio"] = ratio
	}
	if size != "" {
		params["size"] = size
	}
	if len(imgs) > 0 {
		params["images"] = imgs
	}
	if *cf.auth != "" {
		params["uid"] = *cf.auth
	}
	if *cf.config != "" {
		b, err := configBytes(*cf.config)
		if err != nil {
			fmt.Fprintln(os.Stderr, "glm2cli:", err)
			return 2
		}
		params["config"] = json.RawMessage(b)
	}
	// spawn 自身 call：与 core 完全同一条 IPC 路径（§4 纪律）
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "glm2cli:", err)
		return 1
	}
	spec := plugin.Spec{Exe: exe, Args: []string{"call", "--data", dataDir}}
	msg, err := plugin.RunOnce(mainCtx, spec, "images.generate", params, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "glm2cli:", err)
		return 1
	}
	if !msgOK(&msg) {
		printErrObj(msg.Error)
		return 1
	}
	if raw {
		return writePretty(msg.Result)
	}
	var res imagesResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		fmt.Fprintf(os.Stderr, "glm2cli: 解析 result 失败: %v\n原始 result：%s\n", err, truncStr(string(msg.Result), 400))
		return 1
	}
	if noSave {
		return writePretty(msg.Result)
	}
	dir := outDir
	if dir == "" {
		dir = filepath.Join(dataDir, "images")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "glm2cli:", err)
		return 1
	}
	for i, it := range res.Data {
		path, err := saveEntry(dir, res.Created, i, &it)
		if err != nil {
			fmt.Fprintf(os.Stderr, "glm2cli: 第 %d 张落盘失败: %v\n", i+1, err)
			return 1
		}
		fmt.Println(path)
	}
	return 0
}

func readPromptVal(flagVal string) (string, error) {
	if flagVal == "-" || flagVal == "" {
		if flagVal == "" {
			if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				return "", fmt.Errorf("没有提示词：用 --prompt 传入，或从 stdin 管道输入")
			}
		}
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
		return "", fmt.Errorf("提示词为空")
	}
	return flagVal, nil
}

var dataURIRe = regexp.MustCompile(`(?s)^data:([^;]+);base64,(.+)$`)

func parseDataURI(s string) (mime, payload string, err error) {
	m := dataURIRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", fmt.Errorf("--ref data URI 格式非法（应为 data:<mime>;base64,<payload>）")
	}
	return m[1], m[2], nil
}

func configBytes(cfg string) ([]byte, error) {
	if len(cfg) > 1 && cfg[0] == '@' {
		return os.ReadFile(cfg[1:])
	}
	return []byte(cfg), nil
}

func msgOK(m *jsonl.Msg) bool { return m.OK != nil && *m.OK }

func printErrObj(e *jsonl.ErrObj) {
	if e == nil {
		fmt.Fprintln(os.Stderr, "glm2cli: 调用失败")
		return
	}
	fmt.Fprintf(os.Stderr, "glm2cli: [%s/%s] %s\n", e.Kind, e.Scope, e.Message)
}

func writePretty(raw json.RawMessage) int {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err == nil {
		fmt.Println(buf.String())
	} else {
		fmt.Println(string(raw))
	}
	return 0
}

func saveEntry(dir string, created int64, i int, it *imageEntry) (string, error) {
	var raw []byte
	if it.B64 != "" {
		b, err := base64.StdEncoding.DecodeString(it.B64)
		if err != nil {
			return "", err
		}
		raw = b
	} else if it.URL != "" {
		resp, err := imgDL.Get(it.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("下载 %s: HTTP %d", it.URL, resp.StatusCode)
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("产物既无 url 也无 b64_json")
	}
	ext := mimeExtStr(it.MimeType, raw)
	name := fmt.Sprintf("%d-%d%s", created, i, ext)
	return filepath.Join(dir, name), os.WriteFile(filepath.Join(dir, name), raw, 0o644)
}

var imgDL = &http.Client{Timeout: 120 * time.Second}

// mimeExtStr 从 mime 或魔数推扩展名（与 channel.mimeExt 同一套白名单）。
func mimeExtStr(mime string, raw []byte) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	}
	if len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 {
		return ".jpg"
	}
	if len(raw) >= 8 && string(raw[:4]) == "\x89PNG" {
		return ".png"
	}
	return ".jpg"
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
