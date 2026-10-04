<img src="assets/icon.svg" width="64" height="64" alt="">

# cliproxyapi-quota-router

A scheduler plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that picks Codex and Claude accounts by how much quota they would otherwise waste.

## The problem

Say you run three ChatGPT Codex accounts behind CLIProxyAPI. The built-in schedulers are round-robin and fill-first, and neither looks at quota. One account has 72% of its weekly window left and resets in 14 hours. Another has 3% left and resets in 8 hours. Round-robin splits traffic between them evenly. At the reset the first account throws away most of a week's allowance, while the second runs dry early and starts returning 429s.

Sorting by earliest reset doesn't fix this. It sends traffic to the 3% account first, which is the one with almost nothing to lose.

This plugin ranks accounts by quota at risk: unused quota divided by the time left before it resets. It fills the account that would waste the most, skips accounts that are nearly out, and falls back to round-robin when it has no data. If every account is out, it fails the request instead of letting the host pick an empty one.

## How it works

CLIProxyAPI already forwards each upstream response's headers to usage plugins, for failed attempts as well as successful ones. Two providers put the account's quota state in those headers:

- Codex responses carry `X-Codex-Primary-Used-Percent`, `X-Codex-Secondary-Reset-After-Seconds` and so on, and the host converts websocket `codex.rate_limits` events into the same headers.
- Claude subscription responses carry `anthropic-ratelimit-unified-5h-utilization`, `anthropic-ratelimit-unified-7d-reset` and related headers. See [Claude](#claude).

The plugin reads those headers after every request, keeps the quota fields, and drops everything else.

When the host needs a credential it calls the plugin's `scheduler.pick` with the candidates it considers usable. The plugin scores them and returns one auth ID, or hands the decision back to the host.

The plugin never contacts OpenAI or Anthropic and never reads credential files. Every five minutes it calls `host.auth.list` to learn which auths are disabled or waiting on a retry and to forget auths that were removed.

The upstream interfaces this relies on, with the source revision they were read from, are in [docs/upstream.md](docs/upstream.md).

## Install

You need a plugin-capable CLIProxyAPI build. The official Linux `.tar.gz`, macOS and Windows releases work. The `_no-plugin` Linux archives and the FreeBSD build cannot load plugins.

### From a GitHub release

1. Download `cliproxyapi-quota-router_<version>_<os>_<arch>.zip` for your platform from the [releases page](https://github.com/OlivierDijkstra/cliproxyapi-quota-router/releases) and check it against `checksums.txt`.
2. Extract `cliproxyapi-quota-router.so` (Linux), `.dylib` (macOS) or `.dll` (Windows) into the plugin directory, either `plugins/<os>/<arch>/` or `plugins/`.
3. Add the config section below and restart CLIProxyAPI.

Each release also has bare libraries named `cliproxyapi-quota-router-<os>-<arch>.<ext>`. Rename one to `cliproxyapi-quota-router.<ext>` before copying it. The host takes the plugin ID from the file name, so a library named `cliproxyapi-quota-router-linux-amd64.so` would need its config under `cliproxyapi-quota-router-linux-amd64`.

### From a plugin store registry

The release assets follow the layout the CLIProxyAPI plugin store installs from. Until the plugin is listed in the official registry, add a registry of your own:

```yaml
plugins:
  store-sources:
    - "https://example.com/registry.json"
```

with an entry like:

```json
{
  "schema_version": 1,
  "plugins": [{
    "id": "cliproxyapi-quota-router",
    "name": "Quota Router",
    "description": "Routes Codex and Claude requests to the account with the most quota at risk of expiring.",
    "author": "Olivier Dijkstra",
    "repository": "https://github.com/OlivierDijkstra/cliproxyapi-quota-router",
    "license": "MIT",
    "tags": ["Scheduler", "Codex", "Claude", "Quota"]
  }]
}
```

## Configuration

Minimal:

```yaml
plugins:
  enabled: true
  configs:
    cliproxyapi-quota-router:
      enabled: true
      priority: 10
```

Every option with its default is in [config.example.yaml](config.example.yaml). A test keeps that file in sync with the code.

| Key | Default | Meaning |
| --- | --- | --- |
| `mode` | `smart` | `smart` or `headroom`. `expiring-first` is accepted as another name for `smart`. See below. |
| `minimum_headroom` | `0.02` | Skip an account while any window has less than this fraction left. A window at 0% is skipped even when this is `0`. |
| `fallback` | `round-robin` | What to do when quota data can't rank the candidates: `round-robin`, `fill-first`, or `none` to leave the choice to the host. See [Missing and stale data](#missing-and-stale-data) for when the host is allowed to choose. |
| `refresh_interval` | `5m` | How often to re-read host auth state. Minimum 10s. |
| `stale_after` | `30m` | An account with any quota window last reported longer ago is refreshed by sending it one request. |
| `probe_interval` | `1m` | Minimum gap between two probe picks of the same account. This limits probes only, not fallback traffic. |
| `min_reset_horizon` | `1h` | A reset closer than this is scored as if it were this far away. |
| `switch_margin` | `0.15` | Stay on the current account until another one scores 15% higher. |
| `tie_tolerance` | `0.05` | Scores within 5% of the best count as tied. |
| `session_affinity` | `true` | Keep a client session on one account while that account stays usable. |
| `session_affinity_ttl` | `1h` | How long a session binding lives. |
| `across_priorities` | `false` | Rank accounts from every priority tier instead of only the highest one. |
| `log_decisions` | `false` | Log one line per decision through the host logger. |
| `reveal_auth_ids` | `false` | Show raw auth IDs in diagnostics. These are usually file names with an e-mail address in them, so the default is a hash such as `auth-3fa91c02be`. |

Numbers must be finite. `.nan` and `.inf` are rejected, because a NaN headroom would compare false against every quota value and let empty accounts through.

The plugin applies new settings whenever the host sends `plugin.reconfigure`. A config it rejects, such as `mode: fastest`, returns an error naming the key, and the plugin keeps its previous settings. How the host reacts to that error is up to CLIProxyAPI.

## Strategies

### smart

Codex and Claude accounts both report a 5-hour window and a weekly window. For each window the plugin computes

```
urgency = (remaining - minimum_headroom) / max(hours until reset, min_reset_horizon)
```

and the account's score is its highest urgency. With the defaults:

| Account | Remaining | Resets in | Score |
| --- | --- | --- | --- |
| A | 72% | 14 h | 0.050 |
| C | 38% | 2 d | 0.0075 |
| D | 91% | 5 d | 0.0074 |
| B | 3% | 8 h | 0.0013 |

A wins. B resets soonest, but only 1% of usable quota is at stake there.

Two rules keep the number honest:

- A window can only be spent while every window that resets later still has room. If the 5-hour window is full but the weekly window has 6% left, the 5-hour window counts as 6%. Percentages of different windows are compared directly because neither provider says how many requests each window holds.
- A window whose reset time has passed counts as full again, and its next reset is projected from the window length. A credential that was exhausted yesterday becomes usable without waiting for a new response.

### expiring-first

`expiring-first` is the same ranking as `smart`. Sorting by the soonest reset alone would send traffic to the 3%/8h account in the example above, which is the opposite of what the name promises. Weighting each reset by the quota it would wipe out is what "use it before it expires" means once accounts hold different amounts, so the name maps to `smart`. Status output and logs show `smart`.

A nearly empty account can still outrank a full one in `smart` when its reset is close. With 3% left and a reset in ten minutes, 1% of usable quota is at risk within the hour, since `min_reset_horizon` counts the reset as one hour away. That beats 38% spread over two days. It never beats an account like A that is about to lose most of its window, and below `minimum_headroom` it gets no traffic at all.

### headroom

Picks the account with the most quota left in its tightest window. It spreads load evenly and ignores reset times.

### Eligibility

An account is skipped when any of these hold:

- the host reports it as `disabled` or `error`, or `host.auth.list` shows it disabled or with a future retry time,
- a window is at 0% or below `minimum_headroom`. It stays skipped until that window resets, or for `stale_after` if Codex gave no reset time,
- Codex sent `X-Codex-Limit-Reached: true` or `X-Codex-Allowed: false` and no window explains it. It stays skipped until that flag is `stale_after` old.
- Claude rejected the account as a whole and no window explains it. It stays skipped until `anthropic-ratelimit-unified-reset`, or for `stale_after` without one.

The host has already removed accounts that are cooling down for the requested model before the plugin sees the list.

When every candidate is skipped, the plugin rejects the pick with code `quota_exhausted` and the client gets an error. Handing the pick back to the host would let it choose one of the skipped accounts, so there is no setting for that.

### Missing and stale data

A Codex or Claude account without fresh data is probed: the plugin sends it one request so the response headers can tell it the quota. Probes go before ranked accounts, so a newly added account is measured right away. After a probe the account waits `probe_interval` before it can be probed again.

`probe_interval` limits probes, not traffic. While an account waits for its probe's answer, or keeps answering without quota headers, it counts as an account without data. It can still be picked by:

- the fallback, when no other account can be ranked,
- session affinity, when a client session is already bound to it.

So an account that never sends quota headers can get more than one request per `probe_interval`. It never gets ranked traffic ahead of an account with known quota, though.

Codex and Claude can send some quota headers and not others. The plugin updates each window and the limit flag only from a response that carries it, and keeps the rest. A response with only the 5-hour window doesn't clear a depleted weekly window, and `X-Codex-Allowed: true` on its own doesn't make an account look full. A response with only a limit flag still leaves the account without data.

Each window ages on its own. Data counts as stale once any window was last reported more than `stale_after` ago, even if other headers keep arriving: a stream of 5-hour-only responses or bare `X-Codex-Allowed` flags doesn't keep an old weekly figure rankable. A window whose reset time has passed counts as full as of that reset and ages from there. Staleness never lifts a block early: a depleted window with a future reset keeps the account skipped until that reset, however old the report. A depleted window without a reset time stops blocking after `stale_after`, and the account is probed like any other with stale data.

Accounts from providers without a parser (anything other than `codex` and `claude`) are never probed. Neither are API-key credentials (`auth_kind: apikey`, which is what CLIProxyAPI sets for `claude-api-key` and `codex-api-key` config entries), because the public APIs don't send subscription quota headers and a probe would only spend a paid request. If one does send quota headers, it is ranked like any other account. Accounts without data are used only when nothing else can be ranked. When nothing at all can be ranked, the plugin decides based on whether anything was skipped:

- Nothing skipped: the pick goes to the host's built-in `round-robin` or `fill-first` scheduler, or with `fallback: none` the plugin leaves it unhandled and the host uses its own setting.
- Something skipped: the plugin runs the fallback itself over the accounts without data. `fallback: none` runs round-robin here. Either way the host never sees a choice that includes a skipped account.

### Stability

The plugin remembers the last account it chose for each provider and model and keeps using it until another account scores more than `switch_margin` higher. Traffic drains one account at a time instead of bouncing between two that are close. When it does switch, candidates within `tie_tolerance` of the best take turns.

### Session affinity

The plugin derives a session key from the same signals the host uses: request metadata (`canonical_session_id`, `execution_session_id`, `lcp_affinity_session_id`, `derived_session_id`) or client headers (`X-Claude-Code-Session-Id`, `Session-Id`, `X-Session-Id`, `X-Session-Affinity`, `Thread-Id`, `X-Thread-Id`, `X-Conversation-Id`). It hashes the key immediately. A session stays on its account until that account becomes ineligible or the binding expires. Prompt caching depends on this.

CLIProxyAPI turns off its own session affinity while a scheduler plugin is active, which is why the plugin keeps its own.

## Claude

Anthropic attaches unified rate-limit headers to responses for Claude subscription credentials: the OAuth logins CLIProxyAPI stores as auth files, and setup tokens. CLIProxyAPI's Claude executor records them for every attempt, including 429s, and passes them to the plugin with provider `claude`. The plugin reads:

| Header | Meaning |
| --- | --- |
| `anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization` | Share of the 5-hour and 7-day windows used, `0` to `1`. Values above `1` count as empty. |
| `anthropic-ratelimit-unified-5h-reset`, `-7d-reset` | Window reset, unix seconds. |
| `anthropic-ratelimit-unified-5h-status`, `-7d-status` | `allowed`, `allowed_warning` or `rejected`. `rejected` empties the window whatever the utilization says. |
| `anthropic-ratelimit-unified-status`, `-representative-claim`, `-reset` | Whether this request was allowed, which limit decided it, and when that limit resets. |
| `anthropic-ratelimit-unified-overage-status` | Whether extra usage can serve the account once its windows are spent. |

The 5-hour and 7-day windows are shared by every model on the account, so they rank and block the credential. Two buckets are not:

- `7d_oi` (`seven_day_overage_included`) is a per-model weekly bucket, and `overage` is paid extra usage. When a request is rejected only on one of those while the shared windows are fine, the account stays usable. CLIProxyAPI already cools that model down on that credential and leaves it out of the candidates for that model. The plugin applies the same rule CLIProxyAPI uses to tell the two apart (`ClaudeHeadersIndicateUnifiedRateLimitRejection`).
- An account whose windows are spent but whose `overage-status` is `allowed` keeps working, billed as extra usage. The plugin doesn't reject it. It is treated like an account without data: used only when no account with subscription quota left can be ranked.

The header names and value formats come from Anthropic's own client (Claude Code 2.1.289) and from response headers reported in [CLIProxyAPI issue #5915](https://github.com/router-for-me/CLIProxyAPI/issues/5915). Anthropic doesn't document them publicly, so they can change without notice. [docs/upstream.md](docs/upstream.md#claude) has the details.

Claude API keys (`claude-api-key` entries) get only per-minute `anthropic-ratelimit-requests-*` and `-tokens-*` headers. Those are throughput limits, not a quota that expires, so the plugin ignores them and leaves API keys to the fallback.

## Diagnostics

`GET /v0/management/quota-router/status` with your management key returns the active config, cached quota per account, store stats and the last 20 decisions:

```json
{
  "outcome": "selected",
  "selected": "auth-3fa91c02be",
  "reason": "best smart score",
  "candidates": [
    {"auth": "auth-3fa91c02be", "state": "eligible", "score": 0.05, "remaining": 0.72, "next_reset": "..."},
    {"auth": "auth-77d0e1a4c9", "state": "blocked", "reason": "below 2% headroom until 2026-10-06T09:00:00Z"}
  ]
}
```

The status never contains tokens, API keys, cookies, client headers or session IDs. Auth IDs are hashed unless `reveal_auth_ids` is on. With `log_decisions: true` the same summary goes to the CLIProxyAPI log, one line per request.

## Troubleshooting

**Every decision says `delegated` with "no quota data to rank candidates".** No response has carried quota headers yet. Send a request through each account. If it persists, check that the candidates' provider is `codex` or `claude` and that they are OAuth logins rather than API keys, and look at `store.snapshots` in the status output.

**The plugin doesn't appear to load.** Check that `plugins.enabled` is `true`, that the file name is exactly `cliproxyapi-quota-router.<ext>`, and that the CLIProxyAPI build supports plugins. On Linux, a `_no-plugin` build logs that loading requires cgo.

**Two scheduler plugins are installed.** Only one runs: the one with the highest `priority`. Disable the other or raise this plugin's `priority`.

**An account is blocked longer than expected.** The `reason` in the status output gives the reset time the plugin is waiting for. A Codex `Limit-Reached` flag with no depleted window blocks for `stale_after`. A Claude rejection with no depleted window blocks until the `anthropic-ratelimit-unified-reset` it came with.

**A Claude account with spent windows still gets traffic.** Its status shows `extra_usage: true`. Anthropic serves it from extra usage, so the plugin keeps it as a last resort instead of rejecting requests. Turn off extra usage for that account at Anthropic if you don't want to pay for it.

**`plugin.register` fails.** The error names the config key. Durations need a unit (`5m`, not `300`).

**Requests fail with `quota_exhausted`.** Every candidate account is empty, below `minimum_headroom`, disabled or cooling down. The `reason` of each candidate in the status output says which, and when it frees up. Lowering `minimum_headroom` helps only for accounts that still have some quota left.

## Limitations

- Only Codex and Claude are parsed. Other providers are scheduled by the fallback. Adding one means a new `quota.Parser` in `internal/quota` that maps its headers to windows.
- Gemini isn't supported. CLIProxyAPI captures quota signals only from Codex and Claude headers, and its Gemini, Vertex and Antigravity executors pass no remaining-quota figure to usage plugins. Supporting Gemini would mean asking Google's quota endpoints with each account's token, which this plugin avoids. It's a candidate for later work.
- Claude's per-model `7d_oi` bucket is not tracked. It only matters for the model it limits, and the host already handles that model's cooldown.
- Codex's per-model limits (`X-Codex-<name>-*`, `X-Codex-Additional-*`) are ignored because the header names don't say which model they govern.
- Quota data only arrives with traffic. The plugin doesn't poll OpenAI's or Anthropic's usage endpoints, because that would mean reading account tokens.
- When the plugin delegates to a built-in scheduler it doesn't learn which account was picked, so that request doesn't create a session binding.
- Go shared libraries can't be unloaded ([golang/go#11100](https://github.com/golang/go/issues/11100)). On Linux the Go linker marks the library `nodelete`, so the host's `dlclose` does nothing, and the Windows host never releases plugin DLLs. A second load in the same process reuses the old Go runtime and the old plugin state. Restart CLIProxyAPI to upgrade the plugin. Don't rely on hot reload.

## Platforms

| OS | amd64 | arm64 | Notes |
| --- | --- | --- | --- |
| Linux | yes | yes | Built in manylinux2014, needs glibc 2.17 or newer. musl systems can't load it, and neither can the matching `_no-plugin` host. |
| macOS | yes | yes | Built on native runners. |
| Windows | yes | yes | amd64 uses the runner's MinGW gcc, arm64 uses MSYS2 clang. |

Other targets are not built. FreeBSD/amd64 could load a plugin in principle, but CLIProxyAPI doesn't ship a plugin-capable FreeBSD build. The library has its own Go runtime and talks to the host only through the C ABI, so it doesn't need to match the host's Go version.

## Development

Requires Go 1.26 and a C compiler, since the entry point uses cgo.

```sh
make test   # go test -race ./...
make lint   # golangci-lint v2
make build  # shared library for the host platform, into dist/
```

`internal/quota` parses headers and `internal/router` does the scoring, selection and RPC handling. Neither needs cgo. `plugin/` is the thin C ABI layer.

CI (`.github/workflows/ci.yml`) runs on every push to any branch and on every pull request. It runs lint, the tests, a build for all six platforms, and a host smoke test. The smoke test (`scripts/host-smoke.sh`) checks out CLIProxyAPI at the version in `go.mod` and loads the built library through the host's own loader. It registers the plugin, feeds it Codex usage records, asks it for picks, including one that must be rejected, reconfigures it, shuts it down, and on Linux loads it a second time. For Claude it goes through the host end to end: the host's auth manager asks the plugin for a credential, the host's Claude executor calls a fake Anthropic server, and the host's usage pipeline hands the response headers back to the plugin, which must then rank the account with more quota at risk. It runs on linux/amd64, linux/arm64 and darwin/arm64. The Windows and darwin/amd64 libraries are built and checked for exports, but no host loads them in CI.

Releases build the same way. Push a tag such as `v0.2.0` and `.github/workflows/release.yml` tests, builds, runs the smoke test, and publishes the store archives, the bare libraries and `checksums.txt`. The tag sets the version reported to the host.

## License

MIT
