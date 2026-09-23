# effort-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) request-normalizer plugin. For each new user turn it asks a Jev-style classifier how much reasoning effort the turn needs, then sets that effort in a way that keeps the prompt cache intact.

| Upstream | How effort is set | Cache |
|---|---|---|
| Claude (`claude-fable-5-1`, `claude-opus-5-5`) | effort-only system message `{"role":"system","content":[],"output_config":{"effort":…}}` | kept |
| GPT-6 (`gpt-6*` via Codex, Claude-format clients only) | `{"type":"configuration_update","reasoning":{"effort":…}}` input item before the user turn | kept |
| GPT-5.x | not routed: an effort change there rewrites the hidden system prefix | — |

Each turn is classified once, on its first request, and the decision is stored in an append-only JSONL file. Later requests replay the same inserts at the same positions, so history stays append-only.

If you set `/effort` yourself to a non-baseline value, your setting wins. If the classifier fails or times out, that turn gets no insert at all.

## Classifier

Any server that implements TypeSafe's Jev contract works: `POST /v1/systemone` with model `jev-latest` and a `choice` question, returning `answers.effort.choice`. SemIf, Laya and Jev all qualify.

## Build

```sh
go build -buildmode=c-shared -o effort-router.dylib . && mv effort-router.dylib ~/.cli-proxy-api/plugins/
```

Then restart the proxy.

## Config (`plugins.configs.effort-router`)

```yaml
effort-router:
  enabled: true
  jev-url: "http://127.0.0.1:8765/v1/systemone"
  jev-model: "jev-latest"
  # jev-api-key: ...
  timeout-ms: 1500
  models: ["claude-fable-5-1", "claude-opus-5-5", "gpt-6"]  # prefixes
  baseline-effort: "medium"   # Claude Code default; other values = manual /effort
  # state-file: ~/.cli-proxy-api/effort-router.jsonl
```

## Caveats

- Deleting the state file changes past turns once: a cache miss, and on Fable 5.1 the thinking blocks are invalidated.
- Subagents on routed models get routed too.
- GPT-6 routing is skipped for native Responses clients such as Codex CLI, because `/responses/compact` rejects histories that contain `configuration_update` items.
