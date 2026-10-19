package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

func buildAuthData(st storage) authData {
	id := sanitizeName(firstNonEmpty(st.UID, st.Label, "account"))
	label := firstNonEmpty(st.Label, "MiniMax Code")
	return authData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fmt.Sprintf("%s-%s.json", providerKey, id),
		Label:       label,
		StorageJSON: encodeStorage(st),
		Metadata: map[string]any{
			"type":   providerKey,
			"region": st.Region,
			"uid":    st.UID,
		},
		Attributes: map[string]string{
			"region": st.Region,
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

type loginState struct {
	Region     string `json:"region"`
	DeviceCode string `json:"device_code"`
	Verifier   string `json:"verifier"`
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
	stateRaw, _ := json.Marshal(loginState{Region: region, DeviceCode: code.DeviceCode, Verifier: verifier})

	expiresIn := code.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	url := firstNonEmpty(code.VerificationURIComplete, code.VerificationURI)
	return authLoginStartResponse{
		Provider:  providerKey,
		URL:       url,
		State:     base64.RawURLEncoding.EncodeToString(stateRaw),
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).UTC(),
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
	stateRaw, errDecode := base64.RawURLEncoding.DecodeString(req.State)
	if errDecode != nil {
		return authLoginPollResponse{Status: "error", Message: "invalid login state"}, nil
	}
	var state loginState
	if errUnmarshal := json.Unmarshal(stateRaw, &state); errUnmarshal != nil {
		return authLoginPollResponse{Status: "error", Message: "invalid login state"}, nil
	}

	tok, errPoll := pollDeviceToken(ctx, state.Region, state.DeviceCode, state.Verifier)
	if errPoll != nil {
		if isPending(errPoll) {
			return authLoginPollResponse{Status: "pending", Message: "等待浏览器里确认授权"}, nil
		}
		return authLoginPollResponse{Status: "error", Message: errPoll.Error()}, nil
	}

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(time.Hour)
	}
	st := storage{
		Type:         providerKey,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt.Unix(),
		Region:       state.Region,
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
