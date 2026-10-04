# CLIProxyAPI Quota Router

Use Codex and Claude subscription quota before it expires, without routing new requests to accounts that are nearly empty.

This [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) scheduler plugin reads quota/reset headers from responses, ranks accounts by usable quota at risk of expiring, and keeps requests in a session on one account while it remains usable. It falls back to round-robin when quota data is unavailable. It does not read credentials or call provider usage APIs. Other providers use the fallback.

## Install

1. Use a plugin-capable CLIProxyAPI build (not a Linux `_no-plugin` build). Download the library or platform ZIP and `checksums.txt` from [Releases](https://github.com/OlivierDijkstra/cliproxyapi-quota-router/releases); verify the checksum.
2. Put the extracted library in CLIProxyAPI's `plugins/` directory (or `plugins/<os>/<arch>/`). Name it `cliproxyapi-quota-router.so` on Linux, `.dylib` on macOS, or `.dll` on Windows. If using Docker, mount that directory at `/CLIProxyAPI/plugins`.
3. Add this to CLIProxyAPI's `config.yaml` and **restart** CLIProxyAPI (Go shared libraries cannot be safely hot-reloaded):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cliproxyapi-quota-router:
      enabled: true
      priority: 10
      mode: smart
      minimum_headroom: 0.02
      fallback: round-robin
      session_affinity: true
      session_affinity_ttl: 1h
```

`smart` scores the quota left above the minimum headroom against time until reset; `headroom` instead chooses the account with the most quota left. At or above 2% remaining is allowed; below 2% is skipped. A session binding is refreshed on each request and changes if that account is blocked. [All options and defaults](config.example.yaml).

**Paid-usage warning:** If Anthropic extra usage is enabled, an exhausted Claude account remains usable. In this release, session affinity can continue sending that session to the account even when another has subscription quota, potentially incurring charges. Disable Anthropic extra usage or set `session_affinity: false` until this is fixed.

`GET /v0/management/quota-router/status` (with your CLIProxyAPI management key) shows cached quotas and recent decisions; auth IDs are hashed by default. The current release is a prerelease. See [upstream interface notes](docs/upstream.md) for the source-level contract.

MIT licensed.
