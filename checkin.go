package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	methodHostAuthList = "host.auth.list"
	methodHostAuthGet  = "host.auth.get"
)

// hostAuthEntry 是 host.auth.list 返回的账号条目（只取用得到的字段）。
type hostAuthEntry struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Label     string `json:"label"`
	Provider  string `json:"provider"`
	Type      string `json:"type"`
	Disabled  bool   `json:"disabled"`
}

type hostAuthListResponse struct {
	Files []hostAuthEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	JSON      json.RawMessage `json:"json"`
}

type checkinResult struct {
	Account string `json:"account"`
	Status  string `json:"status"`
	Day     int    `json:"day,omitempty"`
	Points  int    `json:"points,omitempty"`
	Expire  string `json:"expire_at,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

var (
	checkinMu       sync.Mutex
	lastCheckinDate string // 北京时间的日期，避免同一自然日重复打点
	lastCheckinAt   time.Time
	lastResults     []checkinResult
	schedulerOnce   sync.Once
)

// listPluginAccounts 通过宿主枚举本插件的账号。
func listPluginAccounts() ([]hostAuthEntry, error) {
	raw, errCall := hostCall(methodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	var out hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	accounts := make([]hostAuthEntry, 0, len(out.Files))
	for _, entry := range out.Files {
		provider := firstNonEmpty(entry.Provider, entry.Type)
		if provider != providerKey || entry.Disabled {
			continue
		}
		accounts = append(accounts, entry)
	}
	return accounts, nil
}

func loadAccountStorage(authIndex string) (storage, error) {
	raw, errCall := hostCall(methodHostAuthGet, map[string]any{"auth_index": authIndex})
	if errCall != nil {
		return storage{}, errCall
	}
	var got hostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		return storage{}, errUnmarshal
	}
	return decodeStorage(got.JSON)
}

// runCheckin 对全部账号执行一次签到。claim 是幂等的：
// claim_result=1 本次领取成功，=2 今天已经领过。
func runCheckin(ctx context.Context) ([]checkinResult, error) {
	accounts, errList := listPluginAccounts()
	if errList != nil {
		return nil, fmt.Errorf("list accounts: %w", errList)
	}
	results := make([]checkinResult, 0, len(accounts))
	for _, entry := range accounts {
		label := firstNonEmpty(entry.Label, entry.Name, entry.ID, entry.AuthIndex)
		st, errLoad := loadAccountStorage(entry.AuthIndex)
		if errLoad != nil {
			results = append(results, checkinResult{Account: label, Status: "error", Detail: errLoad.Error()})
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, errClaim := claimCheckin(callCtx, st)
		cancel()
		if errClaim != nil {
			results = append(results, checkinResult{Account: label, Status: "error", Detail: errClaim.Error()})
			continue
		}
		base, _ := out["base_resp"].(map[string]any)
		if code, ok := base["status_code"].(float64); ok && code != 0 {
			msg, _ := base["status_msg"].(string)
			results = append(results, checkinResult{Account: label, Status: "not-claimable", Detail: fmt.Sprintf("%d %s", int(code), msg)})
			continue
		}
		data, _ := out["data"].(map[string]any)
		result := checkinResult{Account: label, Status: "claimed"}
		if v, ok := data["claim_result"].(float64); ok && int(v) == 2 {
			result.Status = "already"
		}
		if v, ok := data["day_no"].(float64); ok {
			result.Day = int(v)
		}
		if v, ok := data["points"].(float64); ok {
			result.Points = int(v)
		}
		if v, ok := data["expire_at_ms"].(float64); ok && v > 0 {
			result.Expire = time.UnixMilli(int64(v)).Format("2006-01-02 15:04")
		}
		results = append(results, result)
	}

	checkinMu.Lock()
	lastResults = results
	lastCheckinAt = time.Now()
	// 只有至少一个账号给出了真实结果（claimed / already / not-claimable）才把今天记为已完成。
	// 全是 error 时不能记，否则机器睡眠跨过 10:00、醒来时凭据还没续上的那次尝试会把当天
	// 标成已签，之后每 20 分钟的重试全部跳过，积分被静默吞掉。
	if !everyAccountErrored(results) {
		lastCheckinDate = beijingDate()
	}
	checkinMu.Unlock()
	return results, nil
}

// everyAccountErrored 判断这一轮是不是什么都没拿到结果。
// 还没有账号时也算「没结果」：那时重试是零成本的本机调用，不该占用当天的一次性名额。
func everyAccountErrored(results []checkinResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if r.Status != "error" {
			return false
		}
	}
	return true
}

func beijingDate() string {
	loc := time.FixedZone("CST", 8*3600)
	return time.Now().In(loc).Format("2006-01-02")
}

// startCheckinScheduler 每天在北京时间 10:00 之后跑一次；claim 幂等，
// 所以即使进程重启重复执行也只会得到 already。
func startCheckinScheduler() {
	schedulerOnce.Do(func() {
		go func() {
			time.Sleep(20 * time.Second)
			for {
				if beijingDate() != lastCheckinDate && time.Now().In(time.FixedZone("CST", 8*3600)).Hour() >= 10 {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					_, _ = runCheckin(ctx)
					cancel()
				}
				time.Sleep(20 * time.Minute)
			}
		}()
	})
}

// checkinNotice 是当前上游行为的说明：claim 只发放积分，不会触发重置卡获取。
// 上游哪天支持一并领取，改这一处就够了（控制台页与 /status 都读它）。
const checkinNotice = "签到仅发放积分，不会触发重置卡获取"

func checkinState() map[string]any {
	checkinMu.Lock()
	defer checkinMu.Unlock()
	state := map[string]any{
		"last_date": lastCheckinDate,
		"notice":    checkinNotice,
	}
	if !lastCheckinAt.IsZero() {
		state["last_run_at"] = lastCheckinAt.Format(time.RFC3339)
	}
	if lastResults != nil {
		state["results"] = lastResults
	}
	return state
}
