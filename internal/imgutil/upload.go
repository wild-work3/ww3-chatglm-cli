package imgutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 图床上传端点（变量化以便 httptest 注入）。
// 主选 file.sang.pub meitu 通道（落地 meitudata.com 大陆 CDN）；
// 失败回退 icve 通道（落地 file.icve.com.cn）。协议见 大陆可达图床评估.md §1.2：
// ⚠️ 必须 raw binary body（--data-binary 语义），multipart 会被原样存成信封。
var sangUploadURL = "https://file.sang.pub/api/upload"

var uploadChannels = []string{"meitu", "icve"} // 顺序即回退顺序

var uploadClient = &http.Client{Timeout: 120 * time.Second}

// UploadHost 返回图床主机名（供错误文案提示用户，不含协议）。
func UploadHost() string {
	if u, err := url.Parse(sangUploadURL); err == nil {
		return u.Host
	}
	return sangUploadURL
}

// UploadJPEG 把 JPEG 字节传到图床，返回公网直链。
// 逐通道尝试：HTTP 传输错误或 code!=200 都视为该通道失败，换下一个。
func UploadJPEG(jpeg []byte) (url, channel string, err error) {
	name := fmt.Sprintf("glm-ref-%d.jpg", time.Now().UnixMilli())
	var lastErr error
	for _, ch := range uploadChannels {
		req, e := http.NewRequest(http.MethodPost, sangUploadURL, bytes.NewReader(jpeg))
		if e != nil {
			return "", "", e
		}
		req.Header.Set("X-Upload-Type", ch)
		req.Header.Set("X-File-Name", name)
		req.Header.Set("X-File-Size", fmt.Sprint(len(jpeg)))
		req.Header.Set("Content-Type", "image/jpeg")

		resp, e := uploadClient.Do(req)
		if e != nil {
			lastErr = fmt.Errorf("通道 %s 传输失败: %w", ch, e)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("通道 %s HTTP %d: %s", ch, resp.StatusCode, trunc(string(body), 120))
			continue
		}
		// 响应 {"code":200,"msg":"success","data":"https://.../<hash>.jpg"}
		code, direct, e := parseSangResp(body)
		if e != nil {
			lastErr = fmt.Errorf("通道 %s 响应异常: %w", ch, e)
			continue
		}
		if code != 200 || direct == "" {
			lastErr = fmt.Errorf("通道 %s 上传被拒 code=%d", ch, code)
			continue
		}
		if !strings.HasPrefix(direct, "http") {
			lastErr = fmt.Errorf("通道 %s 返回异常直链: %q", ch, trunc(direct, 120))
			continue
		}
		return direct, ch, nil
	}
	return "", "", fmt.Errorf("全部图床通道失败: %w", lastErr)
}

func parseSangResp(body []byte) (code int, data string, err error) {
	// 轻量解析，避免引 json 包级别的强耦合（这里直接用标准库）
	var m struct {
		Code int    `json:"code"`
		Data string `json:"data"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, "", err
	}
	return m.Code, strings.TrimSpace(m.Data), nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
