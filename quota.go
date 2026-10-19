package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// creditDetail 是 credit/details 返回的单个积分批次。
// 签到积分是分批发放的（每天 400，第 4/7 天 1000），每批各自过期。
type creditDetail struct {
	CreditType      int     `json:"credit_type"`
	GrantedAmount   float64 `json:"-"`
	RemainingAmount float64 `json:"-"`
	ConsumedAmount  float64 `json:"-"`
	GrantedAtMS     int64   `json:"granted_at_ms"`
	ExpireAtMS      int64   `json:"expire_at_ms"`
}

type creditDetailsResponse struct {
	TotalCount int            `json:"total_count"`
	BaseResp   map[string]any `json:"base_resp"`
	Details    []struct {
		CreditType      int    `json:"credit_type"`
		GrantedAmount   string `json:"granted_amount"`
		RemainingAmount string `json:"remaining_amount"`
		ConsumedAmount  string `json:"consumed_amount"`
		GrantedAtMS     int64  `json:"granted_at_ms"`
		ExpireAtMS      int64  `json:"expire_at_ms"`
	} `json:"details"`
}

// fetchCreditDetails 读取积分池明细。金额是字符串（"400.00"），要自己转。
func fetchCreditDetails(ctx context.Context, st storage) (*creditDetailsResponse, error) {
	url := siteFor(st.Region).cloud + "/credit/details"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MiniMaxCode")
	req.Header.Set("Authorization", "Bearer "+st.AccessToken)

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("credit details HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out creditDetailsResponse
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, fmt.Errorf("decode credit details: %w", errUnmarshal)
	}
	return &out, nil
}

func parseAmount(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// ---------------------------------------------------------------- quota

type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

type quotaFetchRequest struct {
	AuthIndex    string            `json:"auth_index"`
	AuthID       string            `json:"auth_id"`
	Provider     string            `json:"provider"`
	StorageJSON  []byte            `json:"storage_json,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

type quotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

type quotaFetchResponse struct {
	Subscription *quotaSubscription `json:"subscription,omitempty"`
	Summary      []quotaMetric      `json:"summary,omitempty"`
	Groups       []quotaGroup       `json:"groups,omitempty"`
}

func handleQuotaDescribe() (any, error) {
	return quotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        "MiniMax Code 积分",
		SupportsReset:      false,
	}, nil
}

func handleQuotaFetch(ctx context.Context, request []byte) (any, error) {
	var req quotaFetchRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	st, errStorage := decodeStorage(req.StorageJSON)
	if errStorage != nil {
		return nil, errStorage
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	details, errFetch := fetchCreditDetails(callCtx, st)
	if errFetch != nil {
		return nil, errFetch
	}

	now := time.Now()
	var remaining, granted, consumed float64
	var nextExpire time.Time
	expiringSoon := 0.0
	for _, item := range details.Details {
		remaining += parseAmount(item.RemainingAmount)
		granted += parseAmount(item.GrantedAmount)
		consumed += parseAmount(item.ConsumedAmount)
		if item.ExpireAtMS > 0 {
			exp := time.UnixMilli(item.ExpireAtMS)
			if nextExpire.IsZero() || exp.Before(nextExpire) {
				nextExpire = exp
			}
			// 7 天内过期且还有剩余的积分，单独提示。
			if exp.Before(now.Add(7*24*time.Hour)) {
				expiringSoon += parseAmount(item.RemainingAmount)
			}
		}
	}

	summary := []quotaMetric{
		{Key: "credits_remaining", Label: "积分余额", Value: math.Round(remaining*100) / 100, Unit: "points", Format: "number"},
		{Key: "credits_consumed", Label: "已消耗", Value: math.Round(consumed*100) / 100, Unit: "points", Format: "number"},
		{Key: "credits_granted", Label: "累计获得", Value: math.Round(granted*100) / 100, Unit: "points", Format: "number"},
		{Key: "credit_batches", Label: "积分批次", Value: float64(len(details.Details)), Format: "number"},
	}
	if expiringSoon > 0 {
		summary = append(summary, quotaMetric{Key: "credits_expiring_7d", Label: "7 天内过期", Value: math.Round(expiringSoon*100) / 100, Unit: "points", Format: "number"})
	}

	resp := quotaFetchResponse{
		Subscription: &quotaSubscription{Plan: "MiniMax Code", TierName: "积分池"},
		Summary:      summary,
	}
	if !nextExpire.IsZero() {
		days := int(math.Ceil(nextExpire.Sub(now).Hours() / 24))
		if days < 0 {
			days = 0
		}
		resp.Groups = []quotaGroup{{
			DisplayName: "最近一批积分",
			Buckets: []quotaBucket{{
				Window:            fmt.Sprintf("%d 天后过期", days),
				RemainingFraction: 1,
				ResetTime:         nextExpire.Format(time.RFC3339),
				Description:       nextExpire.Format("2006-01-02 15:04") + " 到期",
			}},
		}}
	}
	return resp, nil
}
