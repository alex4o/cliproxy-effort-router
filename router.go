package main

// effort-router picks the reasoning effort for each new user turn with a Jev-style
// typed decision (SemIf, Laya or real Jev: all speak POST /v1/systemone with
// model jev-latest, so switching is only the host) and
// expresses it as Claude's effort-only system message, which switches effort
// without invalidating the prompt cache.
//
// Claude Code resends the full history on every request and never contains our
// messages, so every past decision is re-inserted at the same position on every
// request. Decisions are persisted; losing them would edit history (cache miss,
// and on Fable 5.1 invalidated thinking blocks).

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

type config struct {
	URL       string   `yaml:"jev-url"`
	Model     string   `yaml:"jev-model"`
	APIKey    string   `yaml:"jev-api-key"`
	TimeoutMS int      `yaml:"timeout-ms"`
	Models    []string `yaml:"models"`          // model-name prefixes to route
	StateFile string   `yaml:"state-file"`      // append-only decision log
	Baseline  string   `yaml:"baseline-effort"` // Claude Code default; any other per-turn value is a manual /effort and wins
}

const noDecision = "-" // classifier failed or turn predates the router: never inject

var (
	mu        sync.Mutex
	cfg       config
	decisions = map[string]string{}
	loaded    string
	classify  = classifyJev // swapped in tests
)

func configure(raw []byte) {
	home, _ := os.UserHomeDir()
	c := config{
		URL:       "http://127.0.0.1:8765/v1/systemone",
		Model:     "jev-latest",
		TimeoutMS: 1500,
		Models:    []string{"claude-fable-5-1", "claude-opus-5-5", "gpt-6"},
		StateFile: filepath.Join(home, ".cli-proxy-api", "effort-router.jsonl"),
		Baseline:  "medium",
	}
	_ = yaml.Unmarshal(raw, &c)
	mu.Lock()
	defer mu.Unlock()
	cfg = c
	if loaded != c.StateFile {
		loadDecisions(c.StateFile)
		loaded = c.StateFile
	}
}

type logLine struct {
	Key     string `json:"key"`
	Effort  string `json:"effort"`
	At      string `json:"at,omitempty"`
	Model   string `json:"model,omitempty"`
	Ms      int64  `json:"ms,omitempty"`
	Preview string `json:"preview,omitempty"`
	Err     string `json:"err,omitempty"`
}

func loadDecisions(path string) {
	decisions = map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var l logLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Key != "" {
			decisions[l.Key] = l.Effort
		}
	}
}

func record(l logLine) {
	decisions[l.Key] = l.Effort
	f, err := os.OpenFile(cfg.StateFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(l)
	f.Write(append(b, '\n'))
}

// userText returns the typed text of a user message, or ok=false for tool
// results and other non-typed turns. Claude Code's <system-reminder> blocks are
// dropped, and cache_control markers never reach the key.
func userText(msg gjson.Result) (string, bool) {
	if msg.Get("role").String() != "user" {
		return "", false
	}
	content := msg.Get("content")
	if content.Type == gjson.String {
		return content.String(), content.String() != ""
	}
	var parts []string
	typed := true
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_result":
			typed = false
			return false
		case "text", "input_text": // Claude, Responses
			if t := block.Get("text").String(); !strings.HasPrefix(t, "<system-reminder>") {
				parts = append(parts, t)
			}
		}
		return true
	})
	text := strings.Join(parts, "\n")
	return text, typed && text != ""
}

func isEffortDirective(msg gjson.Result) bool {
	return msg.Get("role").String() == "system" && msg.Get("output_config.effort").Exists()
}

func assistantTail(msg gjson.Result, n int) string {
	var parts []string
	msg.Get("content").ForEach(func(_, block gjson.Result) bool {
		if t := block.Get("type").String(); t == "text" || t == "output_text" {
			parts = append(parts, block.Get("text").String())
		}
		return true
	})
	if msg.Get("content").Type == gjson.String {
		parts = append(parts, msg.Get("content").String())
	}
	return tail(strings.Join(parts, "\n"), n)
}

func head(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

func key(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:12])
}

func routed(model string) bool {
	for _, prefix := range cfg.Models {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func route(model string, body []byte) []byte {
	mu.Lock()
	defer mu.Unlock()
	if !routed(model) {
		return body
	}
	msgs := gjson.GetBytes(body, "messages").Array()
	finalAssistant := -1
	raws := make([]string, len(msgs))
	for i, msg := range msgs {
		raws[i] = msg.Raw
		if msg.Get("role").String() == "assistant" {
			finalAssistant = i
		}
	}
	inserts := map[int]string{} // message index -> directive placed before it
	lastAssistant := -1
	for i, msg := range msgs {
		if msg.Get("role").String() == "assistant" {
			lastAssistant = i
		}
		text, ok := userText(msg)
		if !ok {
			continue
		}
		// Claude Code 2.1.280 attaches the turn's effort to the system message(s)
		// right after the user message. A non-baseline value is the user's /effort.
		directive := -1
		for j := i + 1; j < len(msgs) && msgs[j].Get("role").String() == "system"; j++ {
			if isEffortDirective(msgs[j]) {
				directive = j
			}
		}
		if directive >= 0 && msgs[directive].Get("output_config.effort").String() != cfg.Baseline {
			continue
		}
		prev := ""
		if lastAssistant >= 0 {
			prev = assistantTail(msgs[lastAssistant], 600)
		}
		effort := decide(model, text, prev, i > finalAssistant)
		if effort == "" || effort == noDecision {
			continue
		}
		at := i
		if directive >= 0 {
			if effort == cfg.Baseline {
				continue
			}
			at = directive + 1 // after Claude Code's own value, so ours applies
		}
		inserts[at] = fmt.Sprintf(`{"role":"system","content":[],"output_config":{"effort":%q}}`, effort)
	}
	out := make([]string, 0, len(raws)+len(inserts))
	for i, raw := range raws {
		if d, ok := inserts[i]; ok {
			out = append(out, d)
		}
		out = append(out, raw)
	}
	if d, ok := inserts[len(raws)]; ok {
		out = append(out, d)
	}
	if len(inserts) == 0 {
		return body
	}
	patched, err := sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(out, ",")+"]"))
	if err != nil {
		return body
	}
	return patched
}

// routeCodex is route for OpenAI Responses bodies (GPT-6 via the Codex
// upstream). Top-level reasoning.effort sits in the hidden system instructions,
// so changing it rewrites the cached prefix; a configuration_update input item
// switches effort from that point on and keeps the prefix. The proxy's Claude
// translator drops Claude Code's per-turn directive, so the only user override
// visible here is a non-baseline top-level effort (/effort), which wins.
func routeCodex(model string, body []byte) []byte {
	mu.Lock()
	defer mu.Unlock()
	if !routed(model) {
		return body
	}
	if e := gjson.GetBytes(body, "reasoning.effort"); e.Exists() && e.String() != cfg.Baseline {
		return body
	}
	items := gjson.GetBytes(body, "input").Array()
	finalAssistant := -1
	for i, it := range items {
		if t := it.Get("type").String(); it.Get("role").String() == "assistant" || t == "function_call" || t == "reasoning" {
			finalAssistant = i
		}
	}
	out := make([]string, 0, len(items)*2)
	inserted, lastAssistant := false, -1
	for i, it := range items {
		if it.Get("role").String() == "assistant" {
			lastAssistant = i
		}
		if text, ok := userText(it); ok && (i == 0 || items[i-1].Get("type").String() != "configuration_update") {
			prev := ""
			if lastAssistant >= 0 {
				prev = assistantTail(items[lastAssistant], 600)
			}
			if effort := decide(model, text, prev, i > finalAssistant); effort != "" && effort != noDecision {
				// Every decided turn carries its own item: the latest one wins, so a
				// medium turn after an xhigh one must say so.
				out = append(out, fmt.Sprintf(`{"type":"configuration_update","reasoning":{"effort":%q}}`, effort))
				inserted = true
			}
		}
		out = append(out, it.Raw)
	}
	if !inserted {
		return body
	}
	patched, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(out, ",")+"]"))
	if err != nil {
		return body
	}
	return patched
}

// decide returns the turn's effort, classifying only on the first request of a
// turn (no reply yet): deciding later would edit history the model has already
// answered from.
func decide(model, text, prev string, mayClassify bool) string {
	k := key(text)
	effort, known := decisions[k]
	if known || !mayClassify {
		return effort
	}
	start := time.Now()
	effort, err := classify(prev, head(text, 1200))
	l := logLine{Key: k, Effort: effort, At: start.Format(time.RFC3339), Model: model,
		Ms: time.Since(start).Milliseconds(), Preview: head(text, 80)}
	if err != nil || effort == "" {
		l.Effort, l.Err = noDecision, fmt.Sprint(err)
	}
	record(l)
	return l.Effort
}

var efforts = map[string]string{
	"low":    "Trivial: a quick question, confirmation, chit-chat, running one command, or a tiny obvious edit.",
	"medium": "Routine: a clear, well-scoped coding task or explanation with obvious steps.",
	"high":   "Substantial: multi-step engineering, debugging, or design that needs careful reasoning.",
	"xhigh":  "Hard: open-ended architecture, subtle debugging, research, or a long autonomous build.",
}

func classifyJev(previous, message string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model": cfg.Model,
		"state": map[string]string{"assistant_previous_reply_tail": previous, "user_message": message},
		"questions": map[string]any{"effort": map[string]any{
			"type":         "choice",
			"instructions": "How much reasoning effort should a coding agent spend on this user message, given the assistant's previous reply?",
			"criteria":     efforts,
		}},
	})
	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jev %d: %s", resp.StatusCode, head(buf.String(), 200))
	}
	// Laya/Jev return {answers}, SemIf wraps it as {code,data:{answers}}.
	choice := gjson.GetBytes(buf.Bytes(), "answers.effort.choice")
	if !choice.Exists() {
		choice = gjson.GetBytes(buf.Bytes(), "data.answers.effort.choice")
	}
	if _, ok := efforts[choice.String()]; !ok {
		return "", fmt.Errorf("jev: unexpected answer %s", head(buf.String(), 200))
	}
	return choice.String(), nil
}
