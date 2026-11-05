// ww3auth_test.go —— authstore 未知键保留与轮换落盘的回归测试（离线）。
package glm

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func makeJWT(exp int64) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp, 10) + `}`))
	return head + "." + pay + ".sig"
}

func TestW3CredentialSaveKeepsUnknown(t *testing.T) {
	dir := t.TempDir()
	auths := filepath.Join(dir, "auths")
	if err := os.MkdirAll(auths, 0o700); err != nil {
		t.Fatal(err)
	}
	// 带未知键的 ww3 嵌套形凭据（模拟真实部署文件）
	doc := `{"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1,
"deviceId":"dev1","machineId":"m1","apiHost":"https://x","extra":{"k":1}},
"account":{"uid":"u1","nickname":"老王"}}`
	name := "glm-u1"
	if err := os.WriteFile(filepath.Join(auths, name+".json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewW3Store(dir)
	a, err := store.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if a.UserID != "u1" || a.Nickname != "老王" {
		t.Fatalf("account 节点未补 uid/nickname: %q %q", a.UserID, a.Nickname)
	}
	c := NewW3Credential(store, name, a)
	// 模拟 refresh 轮换后的凭据状态（access 换成带 exp 的 JWT 形状）
	c.AccessToken = makeJWT(1799999999)
	c.RefreshToken = "rt-new"
	if err := c.Save(); err != nil { // saveFn 接管 → 嵌套落盘
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(auths, name+".json"))
	var back struct {
		Auth struct {
			AccessToken  string         `json:"accessToken"`
			RefreshToken string         `json:"refreshToken"`
			ExpiresAt    int64          `json:"expiresAt"`
			MachineID    string         `json:"machineId"`
			APIHost      string         `json:"apiHost"`
			Extra        map[string]any `json:"extra"`
		} `json:"auth"`
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("回读失败: %v\n%s", err, b)
	}
	if back.Auth.RefreshToken != "rt-new" {
		t.Fatalf("新 refresh_token 未落盘: %s", back.Auth.RefreshToken)
	}
	if back.Auth.MachineID != "m1" || back.Auth.APIHost != "https://x" || back.Auth.Extra["k"] != float64(1) {
		t.Fatalf("未知键丢失: %s", b)
	}
	if back.Auth.ExpiresAt <= 1 {
		t.Fatalf("expiresAt 未更新: %d", back.Auth.ExpiresAt)
	}
	if back.Account.UID != "u1" {
		t.Fatalf("account.uid 丢失: %s", b)
	}
}
