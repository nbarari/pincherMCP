# systemd (Linux user service)

Runs pincher under your user account via `systemd --user`. Suitable for
desktop / workstation installs where you want pincher up at login
without needing root or a container runtime. For production / multi-user
deployments, use [Docker](./docker.md) or [Helm](./helm.md) instead.

Unit file: [`packaging/systemd/pincher.service`](../../packaging/systemd/pincher.service).

## Quickstart

```bash
# 1. Get the binary on PATH (one of these):
#    - homebrew tap (Linux+macOS)
brew tap kwad77/pincher https://github.com/kwad77/homebrew-pincher && brew install pincher
#    - or download from releases
curl -L -o pincher.tar.gz https://github.com/kwad77/pincher/releases/download/v0.75.0/pincher-v0.75.0-linux-amd64.tar.gz
tar -xzf pincher.tar.gz && sudo mv pincher-v0.75.0-linux-amd64 /usr/local/bin/pincher

# 2. Install the user unit
mkdir -p ~/.config/systemd/user
cp packaging/systemd/pincher.service ~/.config/systemd/user/

# 3. Set an auth key (optional but recommended for HTTP)
mkdir -p ~/.config/pincher
echo "PINCHER_HTTP_KEY=$(openssl rand -hex 32)" > ~/.config/pincher/env
chmod 600 ~/.config/pincher/env

# 4. Reload + start
systemctl --user daemon-reload
systemctl --user enable --now pincher

# 5. Verify
systemctl --user status pincher
curl -H "Authorization: Bearer $(grep PINCHER_HTTP_KEY ~/.config/pincher/env | cut -d= -f2)" \
  http://localhost:8080/v1/health
```

## Unit file contents

`packaging/systemd/pincher.service` ships these defaults:

| Field | Value | Tunable |
|---|---|---|
| `ExecStart` | `/usr/local/bin/pincher --http :8080` | Edit to override binary path / port |
| `EnvironmentFile` | `-%h/.config/pincher/env` | Leading `-` makes the file optional |
| `Restart` | `on-failure` | Survives crashes; `always` for stronger guarantees |
| `RestartSec` | `5s` | Backoff |
| `StateDirectory` | `pincher` | Pincher's DB lives at `~/.local/state/pincher/` |
| `WorkingDirectory` | `%S/pincher` | Resolved to `~/.local/state/pincher/` |

Override locally by creating a drop-in:

```bash
mkdir -p ~/.config/systemd/user/pincher.service.d
cat > ~/.config/systemd/user/pincher.service.d/override.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/pincher --http :9090 --supervised
EOF
systemctl --user daemon-reload
systemctl --user restart pincher
```

The empty `ExecStart=` is required by systemd to clear the inherited value
before setting the new one.

## Persistence across reboots

The unit installs at user-scope, so it starts on login by default. To
start the unit at boot (before any user logs in):

```bash
sudo loginctl enable-linger $USER
```

Without lingering, the unit stops when the last session logs out and
re-starts on the next login. Index state survives either way — the DB
lives in `~/.local/state/pincher/`.

## Logs

```bash
journalctl --user -u pincher -f         # follow live
journalctl --user -u pincher --since "1h ago"
journalctl --user -u pincher -p err     # errors only
```

Pincher logs to stdout/stderr via `slog`; journald captures both.

## Upgrading

```bash
# 1. Replace the binary (one of):
brew upgrade pincher
# or re-download from releases as above

# 2. Restart the unit
systemctl --user restart pincher
```

Schema migrations run on startup. The auto-restart-on-drift mechanism
([`docs/troubleshooting.md`](../troubleshooting.md)) means the running
process can swap binaries mid-session for MCP clients that respect
`notifications/tools/list_changed`.

## Uninstall

```bash
systemctl --user disable --now pincher
rm ~/.config/systemd/user/pincher.service
rm -rf ~/.config/systemd/user/pincher.service.d
# Optionally remove state:
rm -rf ~/.local/state/pincher
```

## Related

- [Docker](./docker.md) — containerized alternative
- [Helm chart](./helm.md) — Kubernetes deployment
- [`packaging/README.md`](../../packaging/README.md) — every supported install path
- [`docs/troubleshooting.md`](../troubleshooting.md) — operational issues

---

_Last reviewed: v0.75 (#1334)._
