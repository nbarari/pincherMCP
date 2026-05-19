# Homebrew (macOS + Linux)

Pincher publishes a Homebrew tap formula for both `darwin-arm64` /
`darwin-amd64` (macOS Apple Silicon / Intel) and `linux-amd64`. The
formula lives at [`packaging/homebrew/pincher.rb`](../../packaging/homebrew/pincher.rb)
and ships in the [`kwad77/homebrew-pincher`](https://github.com/kwad77/homebrew-pincher)
tap once each release tag fires the auto-bump workflow.

## Quickstart

```bash
brew tap kwad77/pincher https://github.com/kwad77/homebrew-pincher
brew install pincher
pincher --version
# pincherMCP v0.84.0
```

That's it for the binary. If you also want pincher running as a
managed service:

```bash
brew services start pincher
brew services list | grep pincher
# pincher    started
```

`brew services` wires pincher up with the per-platform daemon manager:
launchd on macOS, systemd on Linux. The service runs
`pincher --http :8080` against the local SQLite store at
`$HOMEBREW_PREFIX/var/pincher/`.

## Setting an HTTP auth key

The formula's service definition reads `PINCHER_HTTP_KEY` from a
per-user env file at `$HOMEBREW_PREFIX/etc/pincher/env`:

```bash
mkdir -p "$(brew --prefix)/etc/pincher"
echo "PINCHER_HTTP_KEY=$(openssl rand -hex 32)" > "$(brew --prefix)/etc/pincher/env"
chmod 600 "$(brew --prefix)/etc/pincher/env"
brew services restart pincher
```

Without the env file, the HTTP gateway runs unauthenticated — fine
for localhost-only use, **not safe for any network-exposed deployment**.

## Verifying

```bash
# Check service status
brew services list | grep pincher

# Hit the health endpoint
curl -H "Authorization: Bearer $(grep PINCHER_HTTP_KEY $(brew --prefix)/etc/pincher/env | cut -d= -f2)" \
  http://localhost:8080/v1/health
```

## Wiring up an MCP client

Pincher running under `brew services` exposes the streamable-HTTP MCP
transport at `http://localhost:8080/mcp` when
`PINCHER_MCP_HTTP_PATH=/mcp` is set in the env file. Most MCP-capable
clients (Claude Code, Cursor, Zed) prefer the stdio transport for
local use — `pincher init --target=<host>` wires them up to the
binary directly without going through the service. See
[`docs/tutorials/`](../tutorials/) for per-client setup.

## Upgrading

```bash
brew update
brew upgrade pincher
brew services restart pincher
```

The on-disk SQLite schema migrates automatically; index state
survives upgrades.

## Uninstall

```bash
brew services stop pincher
brew uninstall pincher
brew untap kwad77/pincher
# Optional: remove state
rm -rf "$(brew --prefix)/var/pincher"
rm -rf "$(brew --prefix)/etc/pincher"
```

## Formula details (for tap maintainers)

The formula auto-bumps from a release-tag webhook — when a tag like
`v0.84.0` is pushed to `kwad77/pincher`, a workflow opens a PR
against the tap repo updating `version`, `sha256`, and download
URLs. Review + merge. The release notes link the auto-bump PR.

Manual bump (if the workflow misfires):

```bash
cd packaging/homebrew
# Edit pincher.rb — bump version, urls, and sha256 for each platform binary
shasum -a 256 pincher-v0.84.0-darwin-arm64.tar.gz
# Copy the result into the appropriate sha256 line
```

## Related

- [Docker](./docker.md) — containerized alternative
- [systemd](./systemd.md) — Linux user service (alternative to brew services)
- [`packaging/README.md`](../../packaging/README.md) — every supported install path
- [Scoop manifest](../../packaging/scoop/pincher.json) — Windows equivalent

---

_Last reviewed: v0.75 (#1334)._
