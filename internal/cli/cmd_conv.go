// cmd_conv.go —— 会话管理便利命令（glm 私有能力，ww3 未定义；保留是用户拍板）。
//
// 事件契约沿用 glm2cli 事件流：list/history 逐条 item + done{count}；
// 其余单 result。--cid/--message-id 全部校验 24 位 hex。
package cli

import (
	"encoding/json"

	"os"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	pcli "wild-work3/chatglm-cli/internal/protocol/cli"
)

func mustCID(v, what string) (string, error) {
	if v == "" {
		return "", glm.NewErr(glm.CUsage, "缺少 %s（--cid / --message-id）", what)
	}
	if !glm.CIDPattern.MatchString(v) {
		return "", glm.NewErr(glm.CUsage, "%s 必须是 24 位 hex：%q", what, v)
	}
	return v, nil
}

func registerConv(app *pcli.App, reg map[string]handler) {
	{
		c := app.Cmd("conv-list", "会话列表（内部翻页一次取全量）")
		agent := c.Str("agent", "", "按 assistant_id 过滤（含别名 main|drawing|read|video|study|search|ppt）")
		grep := c.Str("grep", "", "按标题子串过滤")
		limit := c.Int("limit", 0, "最多返回条数（0=不限，契约 §4）")
		offset := c.Int("offset", 0, "跳过条数（与 --limit 配合分页）")
		cf := addCommon(c)
		reg["conv-list"] = func(*pcli.Command) int { return runConvList(agent, grep, limit, offset, cf) }
	}
	{
		c := app.Cmd("conv-show", "会话详情（标题/agent 等 meta）")
		cid := c.Str("cid", "", "会话 ID（24 位 hex）")
		cf := addCommon(c)
		reg["conv-show"] = func(*pcli.Command) int { return runConvShow(cid, cf) }
	}
	{
		c := app.Cmd("conv-history", "单会话历史（升序全量）")
		cid := c.Str("cid", "", "会话 ID（24 位 hex）")
		agent := c.Str("agent", "", "assistant_id（不匹配时上游返回空列表）")
		raw := c.Bool("raw", false, "附带上游原始 input/output")
		cf := addCommon(c)
		reg["conv-history"] = func(*pcli.Command) int { return runConvHistory(cid, agent, raw, cf) }
	}
	{
		c := app.Cmd("conv-rename", "改会话标题")
		cid := c.Str("cid", "", "会话 ID（24 位 hex）")
		title := c.Str("title", "", "新标题（必填）")
		cf := addCommon(c)
		reg["conv-rename"] = func(*pcli.Command) int { return runConvRename(cid, title, cf) }
	}
	{
		c := app.Cmd("conv-delete", "批量删会话（--cid 可重复）")
		cids := c.StrList("cid", "会话 ID（24 位 hex），可重复")
		yes := c.Bool("yes", false, "确认删除（防误操作）")
		agent := c.Str("agent", "", "assistant_id（上游按 agent 匹配，不匹配则静默无效）")
		cf := addCommon(c)
		reg["conv-delete"] = func(*pcli.Command) int { return runConvDelete(cids, yes, agent, cf) }
	}
	{
		c := app.Cmd("conv-stop", "停止正在生成的回答")
		mid := c.Str("message-id", "", "消息 ID = history_id（24 位 hex）")
		cf := addCommon(c)
		reg["conv-stop"] = func(*pcli.Command) int { return runConvCtl(cid2mid, mid, true, cf) }
	}
	{
		c := app.Cmd("conv-read", "标记消息已读")
		mid := c.Str("message-id", "", "消息 ID = history_id（24 位 hex）")
		cf := addCommon(c)
		reg["conv-read"] = func(*pcli.Command) int { return runConvCtl(cid2read, mid, false, cf) }
	}
}

func newEnv(cf *common) *env {
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	return e
}

func runConvList(agent, grep *string, limit, offset *int, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	aid, err := resolveAgent(*agent, "")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	list, err := c.ConvListAll()
	if err != nil {
		e.fail(err)
		return code(err)
	}
	n, skip := 0, 0
	for _, raw := range list {
		var m struct {
			AssistantID string `json:"assistant_id"`
			Title       string `json:"title"`
		}
		_ = json.Unmarshal(raw, &m)
		if aid != "" && m.AssistantID != aid {
			continue
		}
		if *grep != "" && !strings.Contains(m.Title, *grep) {
			continue
		}
		// --offset 先跳过 N 条，--limit 再截断（均对过滤后的序列生效，契约 §4）
		if *offset > 0 && skip < *offset {
			skip++
			continue
		}
		if *limit > 0 && n >= *limit {
			break
		}
		e.ev(&glm.Event{Type: "item", Cmd: "conv-list", Data: raw})
		n++
	}
	e.result("conv-list", map[string]any{"count": n})
	return 0
}

// convShow 双保险：全量列表里找不到时再单独查标题端点，两边都失败才算错。
func runConvShow(cid *string, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	id, err := mustCID(*cid, "--cid")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	out := map[string]any{"conversation_id": id, "found_in_recent": false}
	list, err := c.ConvListAll()
	if err == nil {
		for _, raw := range list {
			var m struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &m) == nil && m.ID == id {
				var meta map[string]any
				_ = json.Unmarshal(raw, &meta)
				out["found_in_recent"], out["meta"] = true, meta
				out["agent_id"] = meta["assistant_id"]
				break
			}
		}
	}
	if title, err := c.ConvTitle(id); err == nil {
		var m struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(title, &m)
		out["title"] = m.Title
	} else if !out["found_in_recent"].(bool) {
		e.fail(err)
		return code(err)
	}
	e.result("conv-show", out)
	return 0
}

func runConvHistory(cid, agent *string, raw *bool, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	id, err := mustCID(*cid, "--cid")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	aid, err := resolveAgent(*agent, "")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	res, err := c.History(id, aid, nil)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if *raw {
		e.passthrough("conv-history", res)
		return 0
	}
	turns, err := glm.ParseHistory(res, false)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if len(turns) == 0 {
		e.ev(&glm.Event{Type: "warn", Data: mustJSON(map[string]any{
			"code": "history_empty", "message": "会话为空；若确认有历史，多半是 --agent 与会话的 assistant_id 不匹配"})})
	}
	for i := range turns {
		e.ev(&glm.Event{Type: "item", Cmd: "conv-history", Index: i, Data: mustJSON(turns[i])})
	}
	e.result("conv-history", map[string]any{"count": len(turns)})
	return 0
}

func runConvRename(cid, title *string, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	id, err := mustCID(*cid, "--cid")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if strings.TrimSpace(*title) == "" {
		err := glm.NewErr(glm.CUsage, "--title 不能为空")
		e.fail(err)
		return code(err)
	}
	if _, err := c.Rename(id, *title); err != nil {
		e.fail(err)
		return code(err)
	}
	e.result("conv-rename", map[string]any{"ok": true, "conversation_id": id, "title": *title})
	return 0
}

func runConvDelete(cids *[]string, yes *bool, agent *string, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if !*yes {
		err := glm.NewErr(glm.CUsage, "删除不可恢复：加 --yes 确认")
		e.fail(err)
		return code(err)
	}
	ids := make([]string, 0, len(*cids))
	for _, s := range *cids {
		id, err := mustCID(s, "--cid")
		if err != nil {
			e.fail(err)
			return code(err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		err := glm.NewErr(glm.CUsage, "至少给一个 --cid")
		e.fail(err)
		return code(err)
	}
	aid, err := resolveAgent(*agent, "")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if _, err := c.BulkDelete(ids, aid); err != nil {
		e.fail(err)
		return code(err)
	}
	e.result("conv-delete", map[string]any{"ok": true, "deleted": len(ids), "agent_id": aid})
	return 0
}

// runConvCtl conv-stop / conv-read 共用（都是对 message_id 的写操作）。
func runConvCtl(call func(*glm.Client, string) (json.RawMessage, error), mid *string, stop bool, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	id, err := mustCID(*mid, "--message-id")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	raw, err := call(c, id)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	what := "conv-read"
	if stop {
		what = "conv-stop"
	}
	e.result(what, map[string]any{"ok": true, "message_id": id, "raw": raw})
	return 0
}

func cid2mid(c *glm.Client, mid string) (json.RawMessage, error)  { return c.StopStream(mid) }
func cid2read(c *glm.Client, mid string) (json.RawMessage, error) { return c.UpdateStatus(mid) }
