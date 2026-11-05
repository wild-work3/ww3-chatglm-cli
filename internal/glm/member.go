package glm

// member.go 积分与额度（移植自 glm-ext/internal/glm/client.go 的 MemberInfo/UserResource*）。
//
// 清言积分 = 登录即送的免费额度（服务端按天被动发放，无「领取」接口），
// 从 member_info.left_score 读取并换算成「积分」单位（scoreUnit=100）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"wild-work3/chatglm-cli/internal/protocol/meta"
)

// MemberInfo 会员与积分信息（来自 EP_MEMBER_INFO）。
type MemberInfo struct {
	// Score 积分余额，**已换算成「积分」单位**（上游 left_score 单位是「分」，除以 100）。
	Score int64
	// RawScore 上游原始值（单位「分」），保留以便排查。
	RawScore int64
	// LeftToken 剩余 token 额度。
	LeftToken int64
	// ScoreRule 积分规则文案（如「免费用户，登录赠送200积分/天」）。
	ScoreRule string
	// IsMember 是否会员。
	IsMember bool
}

// FetchMemberInfo 拉取会员与积分信息。
func (c *Client) FetchMemberInfo(ctx context.Context) (MemberInfo, error) {
	raw, err := c.Do(http.MethodGet, EP_MEMBER_INFO, nil, nil)
	if err != nil {
		return MemberInfo{}, err
	}
	var m struct {
		LeftScore int64  `json:"left_score"`
		LeftToken int64  `json:"left_token"`
		ScoreRule string `json:"score_rule"`
		IsMember  bool   `json:"is_member"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return MemberInfo{}, fmt.Errorf("glm: 解析 member_info 失败: %w", err)
	}
	return MemberInfo{Score: m.LeftScore / scoreUnit, RawScore: m.LeftScore,
		LeftToken: m.LeftToken, ScoreRule: m.ScoreRule, IsMember: m.IsMember}, nil
}

// UserResource 返回可用积分余额（账号可用额度，会随对话消耗递减，与签到无关）。
func (c *Client) UserResource(ctx context.Context) (int64, error) {
	info, err := c.FetchMemberInfo(ctx)
	if err != nil {
		return 0, err
	}
	return info.Score, nil
}

// UserResourceDetail 返回积分明细。
//
// 清言不提供积分条目明细（无「哪笔积分何时到期」这类接口），故只回一条汇总条目：
// 把当前余额作为一个无到期日的条目，让面板能显示数字而不是空白。
// ExpireAt 留空 = 上游未下发到期时间（前端据此隐藏该列）。
func (c *Client) UserResourceDetail(ctx context.Context) (int64, []meta.ResourceItem, error) {
	info, err := c.FetchMemberInfo(ctx)
	if err != nil {
		return 0, nil, err
	}
	name := "积分余额"
	if info.ScoreRule != "" {
		name = "积分余额（" + info.ScoreRule + "）"
	}
	total := float64(info.Score)
	used := float64(0)
	remain := float64(info.Score)
	items := []meta.ResourceItem{{
		Name: name, Total: &total, Used: &used, Remain: &remain,
		// Key 是稳定伪键，供 core 差分对账。注意**不能**把 score_rule 嵌进 key——
		// 上游改规则文案会导致 key 变化，差分会误报一次 expire + earn（R17）。
		Key: "score", Usable: true,
	}}
	return info.Score, items, nil
}
