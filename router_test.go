package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRouteKeepsDecisionsStableAcrossRequests(t *testing.T) {
	configure([]byte("state-file: " + filepath.Join(t.TempDir(), "s.jsonl")))
	calls := 0
	classify = func(prev, msg string) (string, error) { calls++; return "low", nil }

	sys := `{"type":"text","text":"<system-reminder>ctx</system-reminder>"}`
	// SessionStart hook output trails the first user message as role=system.
	turn1 := `{"model":"claude-fable-5-1","messages":[{"role":"user","content":[` + sys + `,{"type":"text","text":"hi"}]},{"role":"system","content":[{"type":"text","text":"hook"}]}]}`
	out1 := route("claude-fable-5-1", []byte(turn1))
	msgs := gjson.GetBytes(out1, "messages").Array()
	if len(msgs) != 3 || msgs[0].Get("output_config.effort").String() != "low" || calls != 1 {
		t.Fatalf("first request: %s (calls=%d)", out1, calls)
	}

	// Same turn continues with a tool loop, cache_control moved: no reclassify, same insert.
	loop := `{"model":"claude-fable-5-1","messages":[{"role":"user","content":[` + sys + `,{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}]}`
	out2 := route("claude-fable-5-1", []byte(loop))
	if n := len(gjson.GetBytes(out2, "messages").Array()); n != 4 || calls != 1 {
		t.Fatalf("tool loop: %d messages, calls=%d: %s", n, calls, out2)
	}

	// Restart: decisions come back from the state file.
	configure([]byte("state-file: " + cfg.StateFile))
	decisions = map[string]string{}
	loadDecisions(cfg.StateFile)
	if !strings.Contains(string(route("claude-fable-5-1", []byte(loop))), `"effort":"low"`) || calls != 1 {
		t.Fatal("decision not restored from state file")
	}
}

func TestRouteSkipsOverridesFailuresAndOtherModels(t *testing.T) {
	configure([]byte("state-file: " + filepath.Join(t.TempDir(), "s.jsonl")))
	classify = func(prev, msg string) (string, error) { return "", errFake }
	body := `{"messages":[{"role":"user","content":"do the thing"}]}`
	if out := route("claude-fable-5-1", []byte(body)); string(out) != body {
		t.Fatalf("failed classification must not inject: %s", out)
	}
	classify = func(prev, msg string) (string, error) { return "high", nil }
	if out := route("claude-fable-5-1", []byte(body)); string(out) != body {
		t.Fatalf("failed turn must stay uninjected later too: %s", out)
	}
	override := `{"messages":[{"role":"user","content":"new turn"},{"role":"system","content":[{"type":"text","text":"hook"}],"output_config":{"effort":"xhigh"}}]}`
	if out := route("claude-fable-5-1", []byte(override)); string(out) != override {
		t.Fatalf("user /effort must win: %s", out)
	}
	other := `{"messages":[{"role":"user","content":"x"}]}`
	if out := route("claude-sonnet-5", []byte(other)); string(out) != other {
		t.Fatalf("unrouted model touched: %s", out)
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "down" }

var errFake = fakeErr{}

func TestRouteFollowsClaudeCodePerTurnEffort(t *testing.T) {
	configure([]byte("state-file: " + filepath.Join(t.TempDir(), "s.jsonl")))
	classify = func(prev, msg string) (string, error) { return "xhigh", nil }
	body := `{"messages":[{"role":"user","content":"design it"},{"role":"system","content":[{"type":"text","text":"hook"}],"output_config":{"effort":"medium"}}]}`
	out := route("claude-opus-5-5", []byte(body))
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 || msgs[1].Get("output_config.effort").String() != "medium" || msgs[2].Get("output_config.effort").String() != "xhigh" {
		t.Fatalf("router effort must follow Claude Code's untouched one: %s", out)
	}
}

func TestRouteCodexUsesConfigurationUpdates(t *testing.T) {
	configure([]byte("state-file: " + filepath.Join(t.TempDir(), "s.jsonl")))
	calls := 0
	classify = func(prev, msg string) (string, error) { calls++; return map[string]string{"hi": "low", "design": "xhigh"}[msg], nil }
	user := func(s string) string { return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + s + `"}]}` }
	reminder := user(`<system-reminder>\nhook\n</system-reminder>`)
	turn1 := `{"model":"gpt-6-sol","reasoning":{"effort":"medium"},"input":[` + user("hi") + `,` + reminder + `]}`
	out := routeCodex("gpt-6-sol", []byte(turn1))
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 3 || items[0].Get("reasoning.effort").String() != "low" || gjson.GetBytes(out, "reasoning.effort").String() != "medium" {
		t.Fatalf("turn 1: %s", out)
	}
	if again := routeCodex("gpt-6-sol", out); string(again) != string(out) {
		t.Fatalf("second normalize pass must be a no-op: %s", again)
	}
	turn2 := `{"model":"gpt-6-sol","reasoning":{"effort":"medium"},"input":[` + user("hi") + `,` + reminder +
		`,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]},` + user("design") + `]}`
	out = routeCodex("gpt-6-sol", []byte(turn2))
	items = gjson.GetBytes(out, "input").Array()
	if len(items) != 6 || items[0].Get("reasoning.effort").String() != "low" || items[4].Get("reasoning.effort").String() != "xhigh" || calls != 2 {
		t.Fatalf("turn 2 (calls=%d): %s", calls, out)
	}
	override := `{"reasoning":{"effort":"max"},"input":[` + user("hi") + `]}`
	if out := routeCodex("gpt-6-sol", []byte(override)); string(out) != override {
		t.Fatalf("user /effort must win: %s", out)
	}
	if out := routeCodex("gpt-5.6-luna", []byte(turn1)); string(out) != turn1 {
		t.Fatalf("gpt-5 must stay untouched: %s", out)
	}
}
