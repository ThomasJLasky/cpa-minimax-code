package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func handleManagementRegister() (any, error) {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: "GET", Path: "/status", Description: "插件状态、积分余额与签到结果"},
			{Method: "POST", Path: "/checkin", Description: "立即对全部账号执行签到"},
		},
		Resources: []resourceRoute{
			{Path: "/console", Menu: "MiniMax Code", Description: "MiniMax Code 积分与模型"},
			{Path: "/status", Description: "控制台页读取的 JSON（不含凭证）"},
		},
	}, nil
}

func handleManagement(ctx context.Context, request []byte) (any, error) {
	var req managementRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	rawPath := req.Path
	query := ""
	if idx := strings.Index(rawPath, "?"); idx >= 0 {
		query = rawPath[idx+1:]
		rawPath = rawPath[:idx]
	}
	for _, value := range req.Query["action"] {
		query += "&action=" + value
	}
	path := strings.TrimSuffix(rawPath, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		path = path[idx:]
	}
	if path == "" {
		path = "/"
	}

	switch path {
	case "/console", "/":
		// 资源页未鉴权，允许用 ?action=checkin 手动触发一次签到（claim 幂等）。
		if strings.Contains(query, "action=checkin") {
			results, errRun := runCheckin(ctx)
			if errRun != nil {
				return jsonResponse([]byte(fmt.Sprintf(`{"ok":false,"error":%q}`, errRun.Error()))), nil
			}
			body, _ := json.Marshal(map[string]any{"ok": true, "results": results})
			return jsonResponse(body), nil
		}
		return htmlResponse(consoleHTML), nil
	case "/status":
		return jsonResponse(buildStatus(ctx)), nil
	case "/checkin":
		results, errRun := runCheckin(ctx)
		if errRun != nil {
			return jsonResponse([]byte(fmt.Sprintf(`{"ok":false,"error":%q}`, errRun.Error()))), nil
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "results": results})
		return jsonResponse(body), nil
	default:
		return managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(fmt.Sprintf(`{"error":"no route for %s"}`, path)),
		}, nil
	}
}

// buildStatus 汇总模型、积分与签到状态。额度查询失败不影响整体返回。
func buildStatus(ctx context.Context) []byte {
	payload := map[string]any{
		"plugin":  "minimax-cpa-plugin",
		"version": pluginVersion,
		"models":  pluginModels(),
		"checkin": checkinState(),
	}

	quota := map[string]any{}
	accounts, errList := listPluginAccounts()
	if errList != nil {
		quota["error"] = errList.Error()
	} else if len(accounts) == 0 {
		quota["error"] = "还没有账号，请先在 OAuth 登录页添加"
	} else {
		st, errLoad := loadAccountStorage(accounts[0].AuthIndex)
		if errLoad != nil {
			quota["error"] = errLoad.Error()
		} else {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			details, errFetch := fetchCreditDetails(callCtx, st)
			cancel()
			if errFetch != nil {
				quota["error"] = errFetch.Error()
			} else {
				var remaining, consumed float64
				var nextExpire time.Time
				for _, item := range details.Details {
					remaining += parseAmount(item.RemainingAmount)
					consumed += parseAmount(item.ConsumedAmount)
					if item.ExpireAtMS > 0 {
						exp := time.UnixMilli(item.ExpireAtMS)
						if nextExpire.IsZero() || exp.Before(nextExpire) {
							nextExpire = exp
						}
					}
				}
				quota["remaining"] = remaining
				quota["consumed"] = consumed
				quota["batches"] = len(details.Details)
				if !nextExpire.IsZero() {
					quota["next_expire"] = nextExpire.Format("2006-01-02 15:04")
				}
			}
		}
	}
	payload["quota"] = quota

	body, _ := json.MarshalIndent(payload, "", "  ")
	return body
}

func jsonResponse(body []byte) managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(body string) managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       []byte(body),
	}
}

const consoleHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MiniMax Code</title>
<style>
 body{font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;padding:24px;color:#1c1c1e;background:#fafafa}
 h1{font-size:16px;font-weight:500;margin:0 0 4px}
 .sub{color:#8e8e93;font-size:12px;margin-bottom:20px}
 .cards{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:20px}
 .card{background:#fff;border:1px solid #e5e5ea;border-radius:10px;padding:14px 16px;min-width:130px}
 .card .k{color:#8e8e93;font-size:12px;margin-bottom:6px}
 .card .v{font-size:22px;font-weight:500}
 .card .v small{font-size:12px;color:#8e8e93;font-weight:400;margin-left:3px}
 table{border-collapse:collapse;width:100%;background:#fff;border:1px solid #e5e5ea;border-radius:8px;overflow:hidden}
 th,td{text-align:left;padding:10px 12px;border-bottom:1px solid #f0f0f2;font-size:13px}
 th{background:#f7f7f9;font-weight:500;color:#48484a}
 tr:last-child td{border-bottom:none}
 code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:#0a84ff}
 button{border:1px solid #d1d1d6;background:#fff;border-radius:7px;padding:6px 14px;font-size:13px;cursor:pointer}
 button:hover{background:#f2f2f7}
 .row{display:flex;align-items:center;gap:12px;margin-bottom:16px}
 .msg{color:#8e8e93;font-size:12px}
 .notice{background:#fff8e6;border:1px solid #f2e3b8;color:#7a5b00;border-radius:8px;padding:9px 12px;font-size:12.5px;margin:-4px 0 18px}
 .notice:empty{display:none}
</style></head>
<body>
<h1>MiniMax Code</h1>
<div class="sub">用 Code 账号登录态走原生 Anthropic Messages，消耗签到积分池</div>

<div class="row">
  <button id="btn">立即签到</button>
  <span class="msg" id="msg"></span>
</div>
<div class="notice" id="notice"></div>

<div class="cards" id="cards"></div>

<table>
<thead><tr><th>模型 ID</th><th>上游模型</th><th>上下文</th><th>最大输出</th></tr></thead>
<tbody id="rows"><tr><td colspan="4">加载中…</td></tr></tbody>
</table>

<script>
const base = location.pathname.replace(/console$/,'');
function card(k,v,unit){return '<div class="card"><div class="k">'+k+'</div><div class="v">'+v+(unit?'<small>'+unit+'</small>':'')+'</div></div>'}
function load(){
  fetch(base+'status').then(r=>r.json()).then(d=>{
    const q=d.quota||{};
    let html='';
    if(q.error){ html=card('积分','—')+'<div class="card" style="flex:1"><div class="k">状态</div><div class="v" style="font-size:13px;font-weight:400;color:#a32d2d">'+q.error+'</div></div>'; }
    else{ html=card('积分余额',q.remaining||0,'分')+card('已消耗',q.consumed||0,'分')+card('批次',q.batches||0)+(q.next_expire?card('最近到期',q.next_expire):''); }
    document.getElementById('cards').innerHTML=html;
    document.getElementById('rows').innerHTML=d.models.map(m=>
      '<tr><td><code>'+m.ID+'</code></td><td>'+m.Name+'</td><td>'+m.ContextLength+'</td><td>'+m.MaxCompletionTokens+'</td></tr>').join('');
    const ck=d.checkin||{};
    document.getElementById('notice').textContent=ck.notice||'';
    if(ck.last_run_at){
      const rs=(ck.results||[]).map(r=>r.account+': '+r.status+(r.points?' +'+r.points:'')).join('；');
      document.getElementById('msg').textContent='上次签到 '+ck.last_run_at+(rs?' — '+rs:'');
    }
  }).catch(e=>{document.getElementById('rows').innerHTML='<tr><td colspan="4">读取失败：'+e+'</td></tr>'});
}
document.getElementById('btn').onclick=()=>{
  document.getElementById('msg').textContent='签到中…';
  fetch(base+'console?action=checkin').then(r=>r.json()).then(d=>{
    if(!d.ok){document.getElementById('msg').textContent='失败：'+d.error;return}
    document.getElementById('msg').textContent=(d.results||[]).map(r=>r.account+': '+r.status+(r.points?' +'+r.points:'')).join('；')||'没有账号';
    load();
  }).catch(e=>{document.getElementById('msg').textContent='失败：'+e});
};
load();
</script>
</body></html>`
