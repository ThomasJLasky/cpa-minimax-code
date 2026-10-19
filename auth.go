package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const refreshSkew = 5 * time.Minute

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeName(s string) string {
	s = unsafeFileChars.ReplaceAllString(strings.TrimSpace(s), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "account"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// decodeStorage 解析凭证。宿主在 auth.parse / auth.refresh / executor 里
// 传进来的 StorageJSON 都是同一份结构。
func decodeStorage(raw []byte) (storage, error) {
	var st storage
	if len(raw) == 0 {
		return st, fmt.Errorf("credential is empty")
	}
	if errUnmarshal := json.Unmarshal(raw, &st); errUnmarshal != nil {
		return st, fmt.Errorf("decode credential: %w", errUnmarshal)
	}
	if st.AccessToken == "" {
		return st, fmt.Errorf("credential has no access_token")
	}
	if st.Region == "" {
		st.Region = "cn"
	}
	if st.Type == "" {
		st.Type = providerKey
	}
	return st, nil
}

func encodeStorage(st storage) []byte {
	st.Type = providerKey
	raw, _ := json.MarshalIndent(st, "", "  ")
	return raw
}

// refreshIntervalSeconds 给宿主的「至少每这么久检查一次续期」回退值。
//
// 宿主只给内置 provider 配了续期提前量；插件 provider 不在 ProviderRefreshLead 表里，
// Metadata/Attributes 里又没有 refresh_interval_seconds 的话，
// nextRefreshCheckAt 走到 lead==nil 分支直接返回 shouldSchedule=false，
// auth.refresh 永远不会被调度（sdk/cliproxy/auth/auto_refresh_loop.go:409-415）。
// 取剩余寿命的一半：token 1 小时 ⇒ 约 30 分钟检查一次，最迟在到期前 5 分钟
// （NextRefreshAfter）续期。
func refreshIntervalSeconds(st storage) int {
	if st.ExpiresAt <= 0 {
		return 900
	}
	remaining := int(time.Unix(st.ExpiresAt, 0).Sub(time.Now()).Seconds())
	if remaining <= 0 {
		return 300
	}
	interval := remaining / 2
	if interval < 300 {
		interval = 300
	}
	if interval > 1800 {
		interval = 1800
	}
	return interval
}

func buildAuthData(st storage) authData {
	id := sanitizeName(firstNonEmpty(st.UID, st.Label, "account"))
	label := firstNonEmpty(st.Label, "MiniMax Code")
	interval := strconv.Itoa(refreshIntervalSeconds(st))
	return authData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fmt.Sprintf("%s-%s.json", providerKey, id),
		Label:       label,
		StorageJSON: encodeStorage(st),
		// 宿主判断「有没有续期凭据」「什么时候过期」「多久检查一次」看的都是
		// Auth.Metadata，不是 StorageJSON（authAccessToken / authHasRefreshCredential /
		// ExpirationTime / authPreferredInterval）。只把 token 放 StorageJSON 里，
		// 宿主既不会主动续期，也不会在 401 时走 refresh-before-fallback。
		Metadata: map[string]any{
			"type":                     providerKey,
			"region":                   st.Region,
			"uid":                      st.UID,
			"access_token":             st.AccessToken,
			"refresh_token":            st.RefreshToken,
			"expires_at":               st.ExpiresAt,
			"refresh_interval_seconds": interval,
		},
		Attributes: map[string]string{
			"region":                   st.Region,
			"refresh_interval_seconds": interval,
		},
		NextRefreshAfter: nextRefreshAt(st),
	}
}

func nextRefreshAt(st storage) time.Time {
	if st.ExpiresAt <= 0 {
		return time.Now().Add(30 * time.Minute).UTC()
	}
	exp := time.Unix(st.ExpiresAt, 0)
	next := exp.Add(-refreshSkew)
	if next.Before(time.Now()) {
		// 已经过期或即将过期：立刻让宿主再刷一次。
		return time.Now().UTC()
	}
	return next.UTC()
}

// ---------------------------------------------------------------- auth.parse

func handleAuthParse(request []byte) (any, error) {
	var req authParseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	st, errDecode := decodeStorage(req.RawJSON)
	if errDecode != nil {
		return authParseResponse{Handled: false}, nil
	}
	return authParseResponse{Handled: true, Auth: buildAuthData(st)}, nil
}

// ---------------------------------------------------------------- auth.login.start

// pendingLogin 是设备码会话的服务端状态。
//
// 宿主要求 auth.login.start 返回的 State 长度 ≤128 且只含 [A-Za-z0-9._-]
// （internal/api/handlers/management/oauth_sessions.go:ValidateOAuthState），
// 而 device_code + PKCE verifier 一起 JSON+base64 就有 150+ 字符，会被宿主判成
// "invalid oauth state: too long" 并直接 502。所以真实状态留在插件内存里，
// State 只放一个 32 位十六进制的短 token。
type pendingLogin struct {
	Region     string
	DeviceCode string
	Verifier   string
	ExpiresAt  time.Time
}

var (
	loginMu       sync.Mutex
	pendingLogins = map[string]*pendingLogin{}
)

func newLoginToken() (string, error) {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", errRead
	}
	return hex.EncodeToString(buf), nil
}

func savePendingLogin(token string, session *pendingLogin) {
	loginMu.Lock()
	defer loginMu.Unlock()
	// 顺带清理过期会话，避免面板上反复发起登录把 map 撑大。
	for key, existing := range pendingLogins {
		if existing.ExpiresAt.Before(time.Now()) {
			delete(pendingLogins, key)
		}
	}
	pendingLogins[token] = session
}

func lookupPendingLogin(token string) *pendingLogin {
	loginMu.Lock()
	defer loginMu.Unlock()
	session := pendingLogins[token]
	if session == nil || session.ExpiresAt.Before(time.Now()) {
		return nil
	}
	return session
}

func dropPendingLogin(token string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(pendingLogins, token)
}

func handleAuthLoginStart(ctx context.Context, request []byte) (any, error) {
	var req authLoginStartRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	region := "cn"
	if req.Metadata != nil {
		if v, ok := req.Metadata["region"].(string); ok && v != "" {
			region = v
		}
	}
	code, verifier, errStart := startDeviceCode(ctx, region)
	if errStart != nil {
		return nil, errStart
	}
	token, errToken := newLoginToken()
	if errToken != nil {
		return nil, errToken
	}

	expiresIn := code.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).UTC()
	savePendingLogin(token, &pendingLogin{
		Region:     region,
		DeviceCode: code.DeviceCode,
		Verifier:   verifier,
		ExpiresAt:  expiresAt,
	})

	url := firstNonEmpty(code.VerificationURIComplete, code.VerificationURI)
	return authLoginStartResponse{
		Provider:  providerKey,
		URL:       url,
		State:     token,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"user_code": code.UserCode,
			"region":    region,
			"interval":  code.Interval,
		},
	}, nil
}

// ---------------------------------------------------------------- auth.login.poll

func handleAuthLoginPoll(ctx context.Context, request []byte) (any, error) {
	var req authLoginPollRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	token := strings.TrimSpace(req.State)
	session := lookupPendingLogin(token)
	if session == nil {
		return authLoginPollResponse{Status: "error", Message: "登录会话已过期或不存在（宿主重启也会丢弃），请重新发起登录"}, nil
	}

	tok, errPoll := pollDeviceToken(ctx, session.Region, session.DeviceCode, session.Verifier)
	if errPoll != nil {
		if isPending(errPoll) {
			return authLoginPollResponse{Status: "pending", Message: "等待浏览器里确认授权"}, nil
		}
		// 设备码作废或被拒绝，会话没必要再留。
		dropPendingLogin(token)
		return authLoginPollResponse{Status: "error", Message: errPoll.Error()}, nil
	}
	dropPendingLogin(token)

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(time.Hour)
	}
	st := storage{
		Type:         providerKey,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt.Unix(),
		Region:       session.Region,
		Label:        "MiniMax Code",
	}
	return authLoginPollResponse{Status: "success", Auth: buildAuthData(st)}, nil
}

// ---------------------------------------------------------------- auth.refresh

func handleAuthRefresh(ctx context.Context, request []byte) (any, error) {
	var req authRefreshRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	st, errDecode := decodeStorage(req.StorageJSON)
	if errDecode != nil {
		return nil, errDecode
	}
	if st.RefreshToken == "" {
		return nil, fmt.Errorf("credential has no refresh_token; please sign in again")
	}
	tok, errRefresh := refreshAccessToken(ctx, st.Region, st.RefreshToken)
	if errRefresh != nil {
		return nil, errRefresh
	}
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(time.Hour)
	}
	st.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		// refresh_token 每次轮换，必须用新的。
		st.RefreshToken = tok.RefreshToken
	}
	st.ExpiresAt = expiresAt.Unix()
	auth := buildAuthData(st)
	return authRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter}, nil
}
