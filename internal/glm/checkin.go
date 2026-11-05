package glm

// checkin.go 智谱清言「签到」（移植自 glm-ext/internal/glm/checkin.go）。
//
// 机制结论（2026-09-26 以真实账号核实）：清言**没有「签到领取」接口**。
// 积分规则实测为 score_rule = "免费用户，登录赠送200积分/天"，即积分由**服务端按天
// 被动发放**。App 里的每日任务「与伙伴对话」**发一条消息即算完成**，没有领取步骤。
// 故本渠道的「签到」语义 = **保活对话**（既满足每日任务，又维持账号活跃）。
//
// 仍在线的两个真实端点（都要尽力调用，幂等）：
//
//	GET  /chatglm/member-api/member/member_info       积分余额（left_score 单位「分」）
//	POST /chatglm/member-api/member/daily_login_score 每日登录积分（status=0 成功 / 10001 已领取）
//
// 旧 activity-api 签到族已下线（返回「活动已结束」），保留调用作为尽力而为的额外收益，
// 其结果**不决定成败**（积分发放与它无关，当失败上报只会让面板显示无意义红字）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CheckinInfo 签到状态（旧 activity-api 族）。
type CheckinInfo struct {
	TodayStatus int          // 0 = 未签，非 0 = 已签
	TotalDays   int          // 累计打卡天数
	PrizeDays   int          // 抽奖进度：累计天数
	NeedDays    int          // 抽奖进度：所需天数
	PrizeChance int          // 可抽奖次数
	List        []CheckinDay // 打卡明细
}

// CheckinDay 单个打卡日。
type CheckinDay struct {
	Date   string `json:"date"`
	Status int    `json:"status"`
}

// utc8 是清言的业务时区（event_date 用），固定 UTC+8。
var utc8 = time.FixedZone("UTC+8", 8*60*60)

// eventDate 上游 event_date 取值：UTC+8 的 YYYY-MM-DD。
func eventDate(t time.Time) string { return t.In(utc8).Format("2006-01-02") }

// FetchCheckinInfo 查询旧 activity-api 签到状态。
func (c *Client) FetchCheckinInfo(ctx context.Context) (CheckinInfo, error) {
	raw, err := c.Do(http.MethodGet, EP_CHECKIN_INFO,
		map[string]string{"event_date": eventDate(time.Now())}, nil)
	if err != nil {
		return CheckinInfo{}, err
	}
	var r struct {
		CheckInList        []CheckinDay `json:"check_in_list"`
		CheckInTodayStatus int          `json:"check_in_today_status"`
		CheckInTotalDays   int          `json:"check_in_total_days"`
		PrizeDays          int          `json:"prize_days"`
		NeedDays           int          `json:"need_days"`
		PrizeChance        int          `json:"prize_chance"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return CheckinInfo{}, err
	}
	return CheckinInfo{TodayStatus: r.CheckInTodayStatus, TotalDays: r.CheckInTotalDays,
		PrizeDays: r.PrizeDays, NeedDays: r.NeedDays, PrizeChance: r.PrizeChance, List: r.CheckInList}, nil
}

// DailyCheckinReport 执行签到并返回可读结果，供调度器判定「当日是否完成」。
//
// 流程（签到与对话联动，合并为一步）：
//
//	① 先发一条最小对话保活（当日需有对话才能领打卡进度）—— 这是唯一「真失败」来源
//	② 领取每日登录积分（daily_login_score，幂等）
//	③ 读积分余额作为结果展示
//	④ 尽力而为：旧 activity-api 签到若仍在线，顺手试一次
func (c *Client) DailyCheckinReport(ctx context.Context) (string, error) {
	if err := c.keepaliveChat(ctx); err != nil {
		return classifyCheckinErr(err) // 对话失败才是真失败（账号可能失效）
	}
	dailyNote := c.claimDailyLoginScore(ctx)

	msg := "保活对话完成"
	if info, err := c.FetchMemberInfo(ctx); err == nil {
		msg = "保活对话完成，当前积分 " + strconv.FormatInt(info.Score, 10)
		if info.ScoreRule != "" {
			msg += "（" + info.ScoreRule + "）"
		}
	}
	if dailyNote != "" {
		msg += "；" + dailyNote
	}
	if extra := c.tryLegacyCheckin(ctx); extra != "" {
		msg += "；" + extra
	}
	return msg, nil
}

// claimDailyLoginScore 领取每日登录积分，返回可附加到结果文案的说明。
//
// 上游响应语义（实测）：status=0 → 领取成功；status=10001「今日已领取」→ 幂等，非错误。
// 因为 10001 是**正常业务语义**，这里必须用 rawEnvelope（不过业务码校验）而非 Do。
func (c *Client) claimDailyLoginScore(ctx context.Context) string {
	env, err := c.rawEnvelope(http.MethodPost, EP_DAILY_LOGIN_SCORE, map[string]any{})
	if err != nil {
		if isNotFoundErr(err) {
			return "" // 接口未全量开放：静默降级（不误导）
		}
		return "每日登录积分：领取失败"
	}
	switch {
	case env.Status == 0:
		return "每日登录积分已领取"
	case strings.Contains(env.Message, "已领取"):
		return "每日登录积分今日已领"
	default:
		if env.Message != "" {
			return "每日登录积分：" + env.Message
		}
		return "每日登录积分：未领取"
	}
}

// rawEnvelope 发请求并**把非 0 业务码原样返回**（不转 error）。
//
// 用途：某些接口用非 0 的 status 表达**正常业务语义**而非错误（daily_login_score 的
// status=10001「今日已领取」）。仅 HTTP≥400 与非法 JSON 才返回 error。
func (c *Client) rawEnvelope(method, path string, body any) (*envelope, error) {
	raw, status, err := c.doRaw(method, path, nil, body, acceptJSON)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Error{Code: CUpstream, Message: "响应不是合法 JSON（可能被 WAF 拦截页）",
			HTTPStatus: status, Detail: snippet(string(raw)), RequestID: c.LastRID}
	}
	if status >= 400 {
		return nil, &Error{Code: CUpstream, HTTPStatus: status, BizStatus: env.Status,
			Message: "上游错误: " + orText(env.Message, "http error"), RequestID: c.LastRID}
	}
	return &env, nil
}

// envelope 是清言统一响应信封。
type envelope struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
	Rid     string          `json:"rid"`
}

// isNotFoundErr 判断错误是否为「接口不存在」（用于静默降级）。
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.HTTPStatus == http.StatusNotFound
	}
	return strings.Contains(err.Error(), "404")
}

// tryLegacyCheckin 尝试旧 activity-api 签到（已下线，故完全静默失败）。
// 保留理由：活动可能重新上线，届时自动获得额外收益，无需改代码。
func (c *Client) tryLegacyCheckin(ctx context.Context) string {
	info, err := c.FetchCheckinInfo(ctx)
	if err != nil {
		return "" // 活动已下线/未开放：静默忽略
	}
	if info.TodayStatus != 0 {
		return "今日签到已完成"
	}
	if _, err := c.Do(http.MethodPost, EP_CHECKIN, nil,
		map[string]any{"event_date": eventDate(time.Now())}); err != nil {
		return ""
	}
	out := "签到成功"
	if info.TotalDays > 0 {
		out += "（累计打卡 " + strconv.Itoa(info.TotalDays) + " 天）"
	}
	if after, aerr := c.FetchCheckinInfo(ctx); aerr == nil && after.PrizeChance > 0 {
		if prize, derr := c.DrawPrize(ctx); derr == nil && prize != "" {
			out += "，抽中：" + prize
		}
	}
	return out
}

// DrawPrize 抽奖（有次数时调用）。尽力而为，失败不影响签到结果。
func (c *Client) DrawPrize(ctx context.Context) (string, error) {
	raw, err := c.Do(http.MethodPost, EP_DRAW_PRIZE, nil,
		map[string]any{"event_date": eventDate(time.Now())})
	if err != nil {
		return "", err
	}
	var r struct {
		HasPrize  bool   `json:"has_prize"`
		PrizeName string `json:"prize_name"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if !r.HasPrize {
		return "", nil
	}
	return r.PrizeName, nil
}

// keepaliveChat 发一条最小对话以保活账号；对话完成后**立即删除会话**，
// 不在用户清言会话列表里留痕（失败只 warn，不影响保活成功）。
func (c *Client) keepaliveChat(ctx context.Context) error {
	resp, err := c.Chat(ChatOpt{Prompt: "hello", Model: DEFAULT_MODEL, Web: true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 读首段流拿 conversation_id（SSE 首帧即回传），再把流读干让对话真实发生
	// （限制读取量避免长回复拖慢保活）。
	if cid := drainKeepaliveStream(resp.Body); cid != "" {
		if _, err := c.BulkDelete([]string{cid}, DEFAULT_ASSISTANT); err != nil {
			c.warn("glm keepalive 会话删除失败（尽力而为）cid=%s err=%v", cid, err)
		}
	}
	return nil
}

// convIDRe SSE 首帧里的 conversation_id（24 位 hex）。
var convIDRe = regexp.MustCompile(`"conversation_id":"([0-9a-f]{24})"`)

// drainKeepaliveStream 从保活对话流里提取 conversation_id，然后读干流。
// 读不到（异常形态）返回空串，调用方跳过删除。
func drainKeepaliveStream(rc io.Reader) string {
	buf := make([]byte, 0, 16<<10)
	chunk := make([]byte, 4<<10)
	cid := ""
	for len(buf) < 64<<10 {
		n, err := rc.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if cid == "" {
				if m := convIDRe.FindSubmatch(buf); m != nil {
					cid = string(m[1])
				}
			}
		}
		if err != nil {
			break
		}
		if cid != "" && len(buf) >= 16<<10 {
			break // 已拿到 cid 且再读一段（确保对话已产生），不再拖慢保活
		}
	}
	return cid
}

// classifyCheckinErr 把签到错误映射成可读文案（而不是裸 error）。
// 调度器需要区分「可重试」与「已完成」，裸 error 无法表达后者。
func classifyCheckinErr(err error) (string, error) {
	var pe *Error
	if errors.As(err, &pe) {
		if pe.Code == CAuth {
			return "登录态失效，待刷新后重试", err
		}
		return truncateRunes(pe.Message, 120), err
	}
	return truncateRunes(err.Error(), 120), err
}

// truncateRunes 见 ww3login.go（同名工具，rune 安全截断）。
