// 插件测试桩：dlopen 插件，模拟宿主完成 plugin.register + executor.execute。
// 不依赖 CPA 进程，用来在装机前验证 ABI 与上游链路。
package main

/*
#cgo darwin LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

extern int TestHostCallGo(void* ctx, char* method, uint8_t* req, size_t len, cliproxy_buffer* out);
extern void TestHostFreeGo(void* ptr, size_t len);

static int TestHostCall(void* ctx, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	return TestHostCallGo(ctx, (char*)method, (uint8_t*)req, len, out);
}
static void TestHostFree(void* ptr, size_t len) {
	TestHostFreeGo(ptr, len);
}

static void* plug_open(const char* p) { return dlopen(p, RTLD_NOW | RTLD_LOCAL); }
static void* plug_sym(void* h, const char* n) { return dlsym(h, n); }
static const char* plug_err(void) { return dlerror(); }

static int plug_init(void* fn, cliproxy_host_api* host, cliproxy_plugin_api* plug) {
	return ((cliproxy_plugin_init_fn)fn)(host, plug);
}

static int plug_call(cliproxy_plugin_api* api, char* m, uint8_t* req, size_t len, cliproxy_buffer* out) {
	return api->call(m, req, len, out);
}
static void plug_free(cliproxy_plugin_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

static void plug_set_host(cliproxy_host_api* api) {
	api->abi_version = 1;
	api->host_ctx = NULL;
	api->call = TestHostCall;
	api->free_buffer = TestHostFree;
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

var lastHostCall string

//export TestHostCallGo
func TestHostCallGo(ctx unsafe.Pointer, method *C.char, req *C.uint8_t, length C.size_t, out *C.cliproxy_buffer) C.int {
	name := C.GoString(method)
	lastHostCall = name
	var body []byte
	if req != nil && length > 0 {
		body = C.GoBytes(unsafe.Pointer(req), C.int(length))
	}
	reply := []byte(`{"ok":true,"result":{}}`)
	fmt.Printf("  [host] %s %s\n", name, truncate(string(body), 120))
	ptr := C.CBytes(reply)
	out.ptr = ptr
	out.len = C.size_t(len(reply))
	return 0
}

//export TestHostFreeGo
func TestHostFreeGo(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	libPath := os.Args[1]
	handle := C.plug_open(C.CString(libPath))
	if handle == nil {
		fmt.Println("dlopen failed:", C.GoString(C.plug_err()))
		os.Exit(1)
	}

	initFn := C.plug_sym(handle, C.CString("cliproxy_plugin_init"))
	if initFn == nil {
		fmt.Println("missing cliproxy_plugin_init")
		os.Exit(1)
	}

	var host C.cliproxy_host_api
	C.plug_set_host(&host)
	var plugin C.cliproxy_plugin_api
	if rc := C.plug_init(initFn, &host, &plugin); rc != 0 {
		fmt.Println("init failed:", rc)
		os.Exit(1)
	}
	fmt.Printf("init ok: abi=%d\n", uint32(plugin.abi_version))

	call := func(method string, payload []byte) []byte {
		var resp C.cliproxy_buffer
		var p *C.uint8_t
		if len(payload) > 0 {
			p = (*C.uint8_t)(C.CBytes(payload))
		}
		rc := C.plug_call(&plugin, C.CString(method), p, C.size_t(len(payload)), &resp)
		if rc != 0 {
			fmt.Printf("  call %s rc=%d\n", method, int(rc))
		}
		if resp.ptr == nil {
			return nil
		}
		out := C.GoBytes(resp.ptr, C.int(resp.len))
		C.plug_free(&plugin, resp.ptr, resp.len)
		return out
	}

	// login / poll 子命令：驱动设备码授权
	if len(os.Args) > 2 && os.Args[2] == "quota" {
		raw := call("quota.describe", []byte(`{}`))
		var env2 envelope
		_ = json.Unmarshal(raw, &env2)
		fmt.Println("== quota.describe ==", string(env2.Result))
		req, _ := json.Marshal(map[string]any{
			"auth_id": "local", "provider": "minimax-code",
			"storage_json": buildStorage(),
		})
		raw = call("quota.fetch", req)
		_ = json.Unmarshal(raw, &env2)
		if !env2.OK {
			fmt.Println("quota.fetch failed:", env2.Error.Code, env2.Error.Message)
			return
		}
		fmt.Println("== quota.fetch ==")
		fmt.Println(pretty(env2.Result))
		return
	}

	if len(os.Args) > 2 && (os.Args[2] == "login" || os.Args[2] == "poll") {
		if os.Args[2] == "login" {
			req, _ := json.Marshal(map[string]any{"Provider": "minimax-code", "Metadata": map[string]any{"region": "cn"}})
			raw := call("auth.login.start", req)
			var env2 envelope
			_ = json.Unmarshal(raw, &env2)
			if !env2.OK {
				fmt.Println("login start failed:", env2.Error.Code, env2.Error.Message)
				os.Exit(1)
			}
			var resp struct {
				URL   string `json:"URL"`
				State string `json:"State"`
				Meta  struct {
					UserCode string `json:"user_code"`
				} `json:"Metadata"`
			}
			_ = json.Unmarshal(env2.Result, &resp)
			_ = os.WriteFile("/tmp/mm-login-state.json", []byte(resp.State), 0o600)
			fmt.Println("URL:", resp.URL)
			fmt.Println("CODE:", resp.Meta.UserCode)
			return
		}
		stateRaw, errRead := os.ReadFile("/tmp/mm-login-state.json")
		if errRead != nil {
			fmt.Println("no login state, run login first")
			os.Exit(1)
		}
		req, _ := json.Marshal(map[string]any{"Provider": "minimax-code", "State": string(stateRaw)})
		raw := call("auth.login.poll", req)
		var env2 envelope
		_ = json.Unmarshal(raw, &env2)
		if !env2.OK {
			fmt.Println("poll failed:", env2.Error.Code, env2.Error.Message)
			os.Exit(1)
		}
		var resp struct {
			Status  string `json:"Status"`
			Message string `json:"Message"`
			Auth    struct {
				StorageJSON []byte `json:"StorageJSON"`
				FileName    string `json:"FileName"`
				Label       string `json:"Label"`
			} `json:"Auth"`
		}
		_ = json.Unmarshal(env2.Result, &resp)
		fmt.Println("status:", resp.Status, resp.Message)
		if resp.Status == "success" {
			_ = os.WriteFile("/tmp/mm-auth.json", resp.Auth.StorageJSON, 0o600)
			fmt.Println("file:", resp.Auth.FileName, "label:", resp.Auth.Label)
			fmt.Println("saved to /tmp/mm-auth.json")
		}
		return
	}

	// 1) plugin.register
	raw := call("plugin.register", nil)
	var env envelope
	_ = json.Unmarshal(raw, &env)
	fmt.Println("\n== plugin.register ==")
	fmt.Println(pretty(env.Result))
	if len(os.Args) > 2 && os.Args[2] == "register" {
		return
	}

	// 2) auth.identifier
	raw = call("auth.identifier", nil)
	_ = json.Unmarshal(raw, &env)
	fmt.Println("\n== auth.identifier ==", string(env.Result))

	// 3) auth.refresh（本地 token 已过期，先走插件的续期路径）
	storage := buildStorage()
	refreshReq, _ := json.Marshal(map[string]any{
		"AuthID":       "local",
		"AuthProvider": "minimax-code",
		"StorageJSON":  storage,
	})
	fmt.Println("\n== auth.refresh ==")
	raw = call("auth.refresh", refreshReq)
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		fmt.Println("refresh failed:", truncate(string(raw), 400))
		os.Exit(1)
	}
	var refreshResp struct {
		Auth struct {
			StorageJSON []byte `json:"StorageJSON"`
			Label       string `json:"Label"`
		} `json:"Auth"`
		NextRefreshAfter string `json:"NextRefreshAfter"`
	}
	_ = json.Unmarshal(env.Result, &refreshResp)
	fmt.Println("refreshed ok, next:", refreshResp.NextRefreshAfter)
	if errWrite := os.WriteFile("/tmp/refreshed-storage.json", refreshResp.Auth.StorageJSON, 0o600); errWrite != nil {
		fmt.Println("write refreshed storage failed:", errWrite)
	}
	storage = refreshResp.Auth.StorageJSON

	reqBody, _ := json.Marshal(map[string]any{
		"AuthID":       "local",
		"AuthProvider": "minimax-code",
		"Model":        "minimax-m3.1-flash",
		"Format":       "anthropic",
		"Stream":       false,
		"Payload":      []byte(`{"model":"minimax-m3.1-flash","max_tokens":32,"messages":[{"role":"user","content":"只回复两个字：通了"}]}`),
		"StorageJSON":  storage,
	})
	fmt.Println("\n== executor.execute ==")
	raw = call("executor.execute", reqBody)
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		fmt.Println("decode failed:", errUnmarshal, truncate(string(raw), 300))
		os.Exit(1)
	}
	if !env.OK {
		fmt.Println("error:", env.Error.Code, env.Error.Message)
		os.Exit(1)
	}
	var execResp struct {
		Payload []byte `json:"Payload"`
	}
	_ = json.Unmarshal(env.Result, &execResp)
	fmt.Println(truncate(string(execResp.Payload), 900))
}

func pretty(raw json.RawMessage) string {
	var v any
	if errUnmarshal := json.Unmarshal(raw, &v); errUnmarshal != nil {
		return string(raw)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}

// buildStorage 把本机 MiniMax Code 登录态转成插件凭证结构（仅测试用）。
func buildStorage() []byte {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".minimax/auth/prod/cn/mcode-public/auth.json")
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		fmt.Println("read minimax auth failed:", errRead)
		os.Exit(1)
	}
	var doc struct {
		Records map[string]struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAtMs  int64  `json:"expiresAtMs"`
		} `json:"records"`
	}
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		fmt.Println("decode minimax auth failed:", errUnmarshal)
		os.Exit(1)
	}
	for key, rec := range doc.Records {
		st := map[string]any{
			"type":          "minimax-code",
			"access_token":  rec.AccessToken,
			"refresh_token": rec.RefreshToken,
			"expires_at":    rec.ExpiresAtMs / 1000,
			"uid":           key,
			"region":        "cn",
			"label":         "MiniMax Code",
		}
		out, _ := json.Marshal(st)
		return out
	}
	fmt.Println("no record in minimax auth")
	os.Exit(1)
	return nil
}
