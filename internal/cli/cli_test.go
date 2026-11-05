// cli_test.go —— 新命令层的关键行为测试（离线，不碰上游）。
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work3/chatglm-cli/internal/channel"
)

// captureStdout/Stderr 换掉标准流执行 fn，返回捕获内容与退出码。
func capture(t *testing.T, fn func() int) (out, errTxt string, code int) {
	t.Helper()
	of, _ := os.CreateTemp(t.TempDir(), "out")
	ef, _ := os.CreateTemp(t.TempDir(), "err")
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = of, ef
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	code = fn()
	of.Close()
	ef.Close()
	ob, _ := os.ReadFile(of.Name())
	eb, _ := os.ReadFile(ef.Name())
	return string(ob), string(eb), code
}

func TestManifestCommand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	_, _, code := capture(t, func() int { return Main([]string{"manifest", "--out", p}) })
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest 不是 JSON: %v", err)
	}
	if m["id"] != channel.ID || m["kind"] != "channel" {
		t.Fatalf("manifest 身份不对: %v", m)
	}
	caps, _ := m["capabilities"].(map[string]any)
	conv, _ := caps["conversation"].([]any)
	found := false
	for _, v := range conv {
		if v == "chat.generate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("capabilities.conversation 缺 chat.generate: %v", conv)
	}
}

func TestSystemoneNotSupported(t *testing.T) {
	_, errTxt, code := capture(t, func() int { return Main([]string{"systemone"}) })
	if code == 0 {
		t.Fatal("systemone 必须非 0 退出")
	}
	if !strings.Contains(errTxt, "not_supported") {
		t.Fatalf("stderr 缺 not_supported: %q", errTxt)
	}
}

func TestNoPositionalArgs(t *testing.T) {
	_, errTxt, code := capture(t, func() int { return Main([]string{"chat", "positional"}) })
	if code == 0 {
		t.Fatal("位置参数必须被拒绝")
	}
	if !strings.Contains(errTxt, "位置参数") && !strings.Contains(errTxt, "不接受") {
		t.Fatalf("报错应说明位置参数问题: %q", errTxt)
	}
}

func TestSchemaCommand(t *testing.T) {
	out, _, _ := capture(t, func() int { return Main([]string{"schema"}) })
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("schema 输出不是 JSON: %v\n%s", err, out)
	}
}

func TestVersionCommand(t *testing.T) {
	out, _, code := capture(t, func() int { return Main([]string{"version"}) })
	if code != 0 || !strings.Contains(out, VERSION) {
		t.Fatalf("version 输出异常: code=%d out=%s", code, out)
	}
}

func TestResolveAgent(t *testing.T) {
	aid, err := resolveAgent("drawing", "")
	if err != nil || aid != "65a232c082ff90a2ad2f15e2" {
		t.Fatalf("drawing 别名: %v %v", aid, err)
	}
	// --ratio 是生图显式信号：agent 缺省/main 时自动切画图智能体
	aid, _ = resolveAgent("", "1:1")
	if aid != "65a232c082ff90a2ad2f15e2" {
		t.Fatalf("--ratio 未切画图: %v", aid)
	}
	if _, err := resolveAgent("bogus", ""); err == nil {
		t.Fatal("未知别名应报错")
	}
}

func TestValidRatio(t *testing.T) {
	for _, r := range []string{"1:1", "3:4", "4:3", "9:16", "16:9"} {
		if !validRatio(r) {
			t.Fatalf("%s 应合法", r)
		}
	}
	if validRatio("2:1") {
		t.Fatal("2:1 应非法")
	}
}

func TestExtMime(t *testing.T) {
	for path, want := range map[string]string{
		"a.png": "image/png", "b.JPG": "image/jpeg", "c.webp": "image/webp",
	} {
		got, err := extMime(path)
		if err != nil || got != want {
			t.Fatalf("%s: got %v err %v", path, got, err)
		}
	}
	if _, err := extMime("x.tiff"); err == nil {
		t.Fatal("推不出 MIME 必须报错（不猜）")
	}
}

func TestParseDataURI(t *testing.T) {
	mime, payload, err := parseDataURI("data:image/png;base64,aGVsbG8=")
	if err != nil || mime != "image/png" || payload != "aGVsbG8=" {
		t.Fatalf("got %v %v %v", mime, payload, err)
	}
	if _, _, err := parseDataURI("https://x/y.png"); err == nil {
		t.Fatal("非 data URI 应报错")
	}
}
