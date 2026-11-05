// cmd_info.go —— 账号/模型/凭据类便利命令。
//
// user/models/agents ：上游原样或本地表；
// accounts           ：ww3 authstore（<data>/auths/*.json）离线列表/删除；
// auth               ：凭据检查/强制刷新；
// login              ：form 登录编排（auth.begin/finish 的 CLI 直连版）。
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"wild-work3/chatglm-cli/internal/glm"
	pcli "wild-work3/chatglm-cli/internal/protocol/cli"
	"wild-work3/chatglm-cli/internal/protocol/jsonl"
)

func registerInfo(app *pcli.App, reg map[string]handler) {
	{
		c := app.Cmd("user", "当前账号信息（昵称/余额 left_score 分 等）")
		raw := c.Bool("raw", false, "原样输出上游响应")
		cf := addCommon(c)
		reg["user"] = func(*pcli.Command) int { return runUser(raw, cf) }
	}
	{
		c := app.Cmd("models", "模型表（--list 走 ww3 静态表，缺省透传上游 available_models）")
		list := c.Bool("list", false, "输出 ww3 归一模型表（glm/ 前缀命名空间）")
		cf := addCommon(c)
		reg["models"] = func(*pcli.Command) int { return runModels(list, cf) }
	}
	{
		_ = app.Cmd("agents", "内置智能体表（assistant_id 对照）")
		reg["agents"] = func(*pcli.Command) int { return runAgents() }
	}
	{
		c := app.Cmd("accounts", "凭据库列表（离线读 <data>/auths）或删除（--remove --auth NAME）")
		remove := c.Bool("remove", false, "删除指定凭据（--auth 必填）")
		cf := addCommon(c)
		reg["accounts"] = func(*pcli.Command) int { return runAccounts(remove, cf) }
	}
	{
		c := app.Cmd("auth", "凭据检查（缺省）或强制刷新（--refresh）")
		refresh := c.Bool("refresh", false, "强制刷新 access token（会轮换 refresh_token 并落盘）")
		cf := addCommon(c)
		reg["auth"] = func(*pcli.Command) int { return runAuth(refresh, cf) }
	}
	{
		c := app.Cmd("login", "粘贴 refresh_token 登录并落盘凭据（form 范式）")
		token := c.Str("token", "", "refresh_token（缺省从 stdin 读一行）")
		flow := c.Str("flow", "", "续查/取消已有登录流程")
		status := c.Bool("status", false, "查询 --flow 状态")
		cancel := c.Bool("cancel", false, "取消 --flow")
		cf := addCommon(c)
		reg["login"] = func(*pcli.Command) int { return runLogin(token, flow, status, cancel, cf) }
	}
}

func runUser(raw *bool, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	res, err := c.UserInfo()
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if *raw {
		e.passthrough("user", res)
		return 0
	}
	var u struct {
		ID       string `json:"_id"`
		Nickname string `json:"nickname"`
		Avatar   string `json:"avatar"`
		IsGuest  bool   `json:"is_guest"`
		Phone    string `json:"phone"`
		Member   struct {
			LeftScore any `json:"left_score"`
		} `json:"member_info"`
	}
	if err := json.Unmarshal(res, &u); err != nil {
		e.fail(glm.NewErr(glm.CUpstream, "user 响应结构异常: %v", err))
		return code(err)
	}
	e.result("user", map[string]any{"uid": u.ID, "nickname": u.Nickname, "avatar": u.Avatar,
		"is_guest": u.IsGuest, "phone": u.Phone, "left_score": u.Member.LeftScore})
	return 0
}

func runModels(list *bool, cf *common) int {
	e := newEnv(cf)
	if *list { // ww3 静态表：无网络调用
		ch, err := e.open(cf)
		if err != nil {
			e.fail(err)
			return code(err)
		}
		res, eo := ch.ModelsList(e.ctx, nil)
		if eo != nil {
			e.fail(errObj2err(eo))
			return 1
		}
		e.passthrough("models", res)
		return 0
	}
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	res, err := c.Models()
	if err != nil {
		e.fail(err)
		return code(err)
	}
	e.passthrough("models", res)
	return 0
}

func runAgents() int {
	e := &env{ctx: mainCtx, start: time.Now(), out: json.NewEncoder(os.Stdout)}
	e.out.SetEscapeHTML(false)
	e.result("agents", glm.AGENTS())
	return 0
}

func runAccounts(remove *bool, cf *common) int {
	e := newEnv(cf)
	ch, err := e.open(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if *remove {
		if *cf.auth == "" {
			err := glm.NewErr(glm.CUsage, "--remove 需要 --auth NAME 指定要删除的凭据")
			e.fail(err)
			return code(err)
		}
		res, eo := ch.AccountsRemove(e.ctx, mustJSON(map[string]any{"auth_file": *cf.auth}))
		if eo != nil {
			e.fail(errObj2err(eo))
			return 1
		}
		e.passthrough("accounts", res)
		return 0
	}
	res, eo := ch.AccountsList(e.ctx, nil)
	if eo != nil {
		e.fail(errObj2err(eo))
		return 1
	}
	e.passthrough("accounts", res)
	return 0
}

func runAuth(refresh *bool, cf *common) int {
	e := newEnv(cf)
	_, c, err := e.client(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	if err := c.EnsureToken(*refresh); err != nil {
		e.fail(err)
		return code(err)
	}
	cr := c.Cred
	view := map[string]any{
		"source":         cr.Source,
		"user_id":        cr.UserID,
		"device_id":      cr.DeviceID,
		"access_exp":     expStr(cr.AccessExp()),
		"refresh_exp":    expStr(cr.RefreshExp()),
		"access_expired": cr.AccessExpired(),
		"refresh_token":  mask(cr.RefreshToken),
		"access_token":   mask(cr.AccessToken),
		"can_persist":    cr.Source != "" && cr.Source != "env",
	}
	e.result("auth", view)
	return 0
}

func runLogin(token, flow *string, status, cancel *bool, cf *common) int {
	e := newEnv(cf)
	ch, err := e.open(cf)
	if err != nil {
		e.fail(err)
		return code(err)
	}
	lm := ch.Logins()
	// 状态/取消子姿势：--flow 必填
	if *status || *cancel {
		if *flow == "" {
			err := glm.NewErr(glm.CUsage, "--status/--cancel 需要 --flow ID")
			e.fail(err)
			return code(err)
		}
		var f *glm.LoginFlow
		if *cancel {
			f, err = lm.Cancel(*flow)
		} else {
			f, err = lm.Status(*flow)
		}
		if err != nil {
			e.fail(err)
			return code(err)
		}
		e.result("login", f)
		return flowExit(f)
	}
	// 主姿势：begin → 粘贴 → finish
	f, err := lm.Begin("")
	if err != nil {
		e.fail(err)
		return code(err)
	}
	fmt.Fprintf(os.Stderr, "登录流程 %s（%s）\n%s\n", f.ID, f.State, f.Hint)
	tok := *token
	if tok == "" {
		fmt.Fprintln(os.Stderr, "粘贴 refresh_token 后回车：")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		tok = strings.TrimSpace(line)
	}
	if tok == "" {
		err := glm.NewErr(glm.CUsage, "没有提交 refresh_token；流程已保留，可用 login --status --flow %s 续查", f.ID)
		e.fail(err)
		return code(err)
	}
	f, err = lm.Finish(e.ctx, f.ID, map[string]string{"refresh_token": tok}, ch.Store())
	if err != nil {
		e.fail(err)
		return code(err)
	}
	e.result("login", f)
	return flowExit(f)
}

func flowExit(f *glm.LoginFlow) int {
	switch f.State {
	case "success":
		return 0
	case "failed":
		return 1
	default:
		return 0 // waiting/cancelled/expired 属过程状态
	}
}

func expStr(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// errObj2err 把 ww3 ErrObj 还原成 glm.Error 以复用事件/退出码原语。
func errObj2err(eo *jsonl.ErrObj) error {
	return glm.NewErr(glm.CUpstream, "[%s/%s] %s", eo.Kind, eo.Scope, eo.Message)
}
