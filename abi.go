package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ponytail: ABI glue copied from dense-system; the logic lives in router.go.

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  struct {
		RequestNormalizer bool `json:"request_normalizer"`
	} `json:"capabilities"`
}

// pluginVersion is set at release build time with -X main.pluginVersion.
var pluginVersion = "0.0.0-dev"

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr) // free(NULL) is a no-op
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		_ = json.Unmarshal(request, &lifecycle)
		configure(lifecycle.ConfigYAML)
		reg := registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{
			Name:             "effort-router",
			Version:          pluginVersion,
			Author:           "alex4o",
			GitHubRepository: "https://github.com/alex4o/cliproxy-effort-router",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "jev-url", Type: pluginapi.ConfigFieldTypeString, Description: "Jev-compatible classifier endpoint (POST /v1/systemone): SemIf, Laya or api.typesafe.ai."},
				{Name: "jev-model", Type: pluginapi.ConfigFieldTypeString, Description: "Classifier model name, default jev-latest."},
				{Name: "jev-api-key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer key for the classifier; empty for local servers."},
				{Name: "timeout-ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Classifier timeout; on timeout the turn is left untouched. Default 1500."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model-name prefixes to route. Supported: Claude with per-turn effort (claude-fable-5-1, claude-opus-5-5) and gpt-6."},
				{Name: "baseline-effort", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"low", "medium", "high", "xhigh", "max"}, Description: "Client default effort; any other request effort counts as a manual /effort and wins. Default medium (Claude Code)."},
				{Name: "state-file", Type: pluginapi.ConfigFieldTypeString, Description: "Append-only decision log; keeps past turns stable across restarts. Default ~/.cli-proxy-api/effort-router.jsonl."},
			},
		}}
		reg.Capabilities.RequestNormalizer = true
		return okEnvelope(reg)
	case pluginabi.MethodRequestNormalize:
		var req pluginapi.RequestTransformRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		body := req.Body
		switch req.ToFormat {
		case "claude":
			body = route(req.Model, body)
		case "codex":
			// Only Claude Code traffic: native Responses clients (Codex CLI) may call
			// /responses/compact, which rejects histories with configuration_update.
			if req.FromFormat == "claude" {
				body = routeCodex(req.Model, body)
			}
		}
		return okEnvelope(pluginapi.PayloadResponse{Body: body})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil {
		return
	}
	response.ptr = C.CBytes(raw) // aborts on OOM rather than returning NULL
	response.len = C.size_t(len(raw))
}
