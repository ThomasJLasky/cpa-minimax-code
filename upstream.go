package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clientID  = "mcode-public"
	oauthScope = "agent.default"
	audience   = "agent-backend"

	// signSalt / signSuffix 是 MiniMax Code 客户端给签到请求签名用的固定常量，
	// 不是凭证：x-signature = md5(秒 + salt + body)，yy = md5(encodeURIComponent(url) + "_" + body + md5(ms) + suffix)。
	signSalt   = "I*7Cf%WZ#S&%1RlZJ&C2"
	signSuffix = "ooui"

	// 客户端指纹，与 Code 桌面端保持一致。
	desktopUA = "MiniMaxAgent"
	mcodeVer  = "0.6.3"
)

type site struct {
	account string
	llm     string
	cloud   string
}

func siteFor(region string) site {
	if strings.EqualFold(region, "global") {
		return site{
			account: "https://account.minimax.io",
			llm:     "https://agent.minimax.io/mavis/api/v1/llm/v1",
			cloud:   "https://agent.minimax.io/minimax-cloud/api/v1",
		}
	}
	return site{
		account: "https://account.minimax.cn",
		llm:     "https://agent.minimax.cn/mavis/api/v1/llm/v1",
		cloud:   "https://agent.minimax.cn/minimax-cloud/api/v1",
	}
}

var httpClient = &http.Client{Timeout: 0}

func requestTimeout() time.Duration { return 90 * time.Second }

// ---------------------------------------------------------------- OAuth

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

func pkcePair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func startDeviceCode(ctx context.Context, region string) (*deviceCodeResponse, string, error) {
	verifier, challenge, errPKCE := pkcePair()
	if errPKCE != nil {
		return nil, "", errPKCE
	}
	body, _ := json.Marshal(map[string]any{
		"client_id":             clientID,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"scope":                 oauthScope,
		"audience":              audience,
	})
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, siteFor(region).account+"/oauth2/device/code", bytes.NewReader(body))
	if errReq != nil {
		return nil, "", errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, "", errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("device code HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out deviceCodeResponse
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode device code: %w (%s)", errUnmarshal, truncate(string(raw), 200))
	}
	if out.DeviceCode == "" {
		return nil, "", fmt.Errorf("device code response missing device_code: %s", truncate(string(raw), 300))
	}
	return &out, verifier, nil
}

func pollDeviceToken(ctx context.Context, region, deviceCode, verifier string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, siteFor(region).account+"/oauth2/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out tokenResponse
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 400 || out.AccessToken == "" {
		if out.Error != "" {
			return nil, &oauthPendingError{code: out.Error, message: out.ErrorDesc}
		}
		return nil, fmt.Errorf("token HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return &out, nil
}

// oauthPendingError 表示设备码还没被确认（RFC 8628 的 authorization_pending / slow_down）。
type oauthPendingError struct {
	code    string
	message string
}

func (e *oauthPendingError) Error() string { return e.code + ": " + e.message }

func isPending(err error) bool {
	pe, ok := err.(*oauthPendingError)
	if !ok {
		return false
	}
	return pe.code == "authorization_pending" || pe.code == "slow_down"
}

func refreshAccessToken(ctx context.Context, region, refreshToken string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	form.Set("scope", oauthScope)
	form.Set("audience", audience)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, siteFor(region).account+"/oauth2/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out tokenResponse
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 400 || out.AccessToken == "" {
		return nil, fmt.Errorf("refresh HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return &out, nil
}

// ---------------------------------------------------------------- 对话

// messagesURL 返回 Anthropic Messages 端点。
func messagesURL(region string) string { return siteFor(region).llm + "/messages" }

func modelsURL(region string) string { return siteFor(region).llm + "/models" }

// newMessagesRequest 构造发给 mavis 网关的请求：Bearer 走 Authorization，
// x-api-key 只是占位（网关忽略），UA 与 Agent 头必须与 Code 客户端一致。
func newMessagesRequest(ctx context.Context, region, token string, payload []byte) (*http.Request, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL(region), bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json, */*")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-api-key", "sk-xxx")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Mavis-Agent-Id", "main")
	_, offsetSec := time.Now().Zone()
	req.Header.Set("X-Mavis-Timezone-Offset", strconv.Itoa(offsetSec))
	req.Header.Set("anthropic-version", "2023-06-01")
	return req, nil
}

// ---------------------------------------------------------------- 签到

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// signinQuery 复刻 Code 客户端的查询串（按 key 字典序，含时区与 user_id 归因）。
func signinQuery(st storage, nowMS int64) string {
	_, offsetSec := time.Now().Zone()
	entries := [][2]string{
		{"app_id", "3001"},
		{"biz_id", "3"},
		{"browser_name", "mcode"},
		{"client", "mcode"},
		{"device_id", "0"},
		{"device_platform", "web"},
		{"desktop_version", mcodeVer},
		{"is_desktop", "1"},
		{"lang", langFor(st.Region)},
		{"os_name", "darwin"},
		{"sys_language", langFor(st.Region)},
		{"timezone_offset", strconv.Itoa(offsetSec)},
		{"unix", strconv.FormatInt(nowMS, 10)},
		{"user_id", firstNonEmpty(st.UID, "0")},
		{"version_code", "22201"},
	}
	// 按 key 排序（上面已按字典序手写，保持稳定）。
	q := url.Values{}
	for _, kv := range entries {
		q.Set(kv[0], kv[1])
	}
	return q.Encode()
}

func langFor(region string) string {
	if strings.EqualFold(region, "global") {
		return "en"
	}
	return "zh"
}

// claimCheckin 直接调用 claim（status 面板的 days 数组会返回空，claim 才是权威）。
func claimCheckin(ctx context.Context, st storage) (map[string]any, error) {
	nowMS := time.Now().UnixMilli()
	query := signinQuery(st, nowMS)
	path := "/signin/claim?" + query
	full := siteFor(st.Region).cloud + path
	sec := nowMS / 1000
	body := "{}"

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MiniMaxCode")
	req.Header.Set("Authorization", "Bearer "+st.AccessToken)
	req.Header.Set("x-timestamp", strconv.FormatInt(sec, 10))
	req.Header.Set("x-signature", md5Hex(strconv.FormatInt(sec, 10)+signSalt+body))
	req.Header.Set("yy", md5Hex(url.QueryEscape(path)+"_"+body+md5Hex(strconv.FormatInt(nowMS, 10))+signSuffix))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkin HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, fmt.Errorf("decode checkin: %w", errUnmarshal)
	}
	return out, nil
}

// ---------------------------------------------------------------- 小工具

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------- 错误分类

// anthropicErrorType 把上游 HTTP 状态映射成 Anthropic 的 error.type，
// 宿主的 claudeErrorDetailFromText 会解析这段 JSON 并原样透给客户端。
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402:
		return "billing_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 504:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

// upstreamErrorMessage 从上游错误体里挖出可读信息（MiniMax 常见 {code,message} 或 base_resp）。
func upstreamErrorMessage(body string) string {
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) == nil {
		for _, key := range []string{"message", "msg"} {
			if v, ok := doc[key].(string); ok && v != "" {
				return v
			}
		}
		if e, ok := doc["error"].(map[string]any); ok {
			if v, ok := e["message"].(string); ok && v != "" {
				return v
			}
		}
		if base, ok := doc["base_resp"].(map[string]any); ok {
			if v, ok := base["status_msg"].(string); ok && v != "" {
				return v
			}
		}
	}
	return truncate(body, 300)
}

// upstreamError 返回一个「消息本身就是 Anthropic 错误 JSON」的 error。
//
// 措辞里的 "status %d" 是有意为之：宿主的 isUnauthorizedError 只认 StatusCode==401
// 或文本含 "status 401" / "401 unauthorized"（sdk/cliproxy/auth/conductor_cooldown.go:1531），
// 而插件错误只能以字符串跨 C ABI 传回、StatusCode 拿不到。写成 "status 401" 才能让宿主
// 在收到 401 时先调 auth.refresh 再重试，而不是直接把账号判死、整个 provider 变成
// auth_unavailable。
func upstreamError(status int, body string) error {
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": fmt.Sprintf("minimax upstream status %d: %s", status, upstreamErrorMessage(body)),
		},
	}
	raw, _ := json.Marshal(payload)
	return fmt.Errorf("%s", string(raw))
}

// upstreamErrorFrame 用于流式：发一帧标准的 event: error。
func upstreamErrorFrame(status int, body string) []byte {
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": fmt.Sprintf("minimax upstream status %d: %s", status, upstreamErrorMessage(body)),
		},
	}
	raw, _ := json.Marshal(payload)
	return []byte("event: error\ndata: " + string(raw) + "\n\n")
}
