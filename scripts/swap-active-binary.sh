#!/usr/bin/env bash
# swap-active-binary.sh — replace the currently-installed pincher binary
# with the freshly-built one in this repo (#705).
#
# Why this exists: on Windows the OS holds an exclusive file lock on every
# running .exe, so `cp pincher.exe $(which pincher)` fails with
# "Device or resource busy" while the supervisor is alive. The fix is the
# rename-out trick: move the running exe to a `.old` sibling (Windows
# tracks open handles by inode, not path, so the running process keeps
# working) and copy the new build into the freed path. The supervisor's
# auto-restart-on-drift picks up the new binary on the next tool call.
#
# Usage:
#   bash scripts/swap-active-binary.sh                    # auto-detect target
#   bash scripts/swap-active-binary.sh --target=PATH     # explicit target
#   bash scripts/swap-active-binary.sh --source=PATH     # override source
#
# Defaults:
#   --source: ./pincher.exe (Windows) or ./pincher (Unix), built by `make build`
#   --target: first `pincher` / `pincher.exe` resolved via PATH
#
# After a successful swap, prints both versions and exits 0.

set -euo pipefail

case "$(uname -s 2>/dev/null || echo Windows)" in
    MINGW*|CYGWIN*|MSYS*|Windows*) IS_WINDOWS=1; EXE_SUFFIX=.exe ;;
    *)                             IS_WINDOWS=0; EXE_SUFFIX= ;;
esac

SOURCE=""
TARGET=""

for arg in "$@"; do
    case "$arg" in
        --source=*) SOURCE="${arg#--source=}" ;;
        --target=*) TARGET="${arg#--target=}" ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "swap-active-binary: unknown arg: $arg" >&2
            exit 2
            ;;
    esac
done

if [[ -z "$SOURCE" ]]; then
    SOURCE="./pincher${EXE_SUFFIX}"
fi
if [[ ! -x "$SOURCE" ]]; then
    echo "swap-active-binary: source binary not found or not executable: $SOURCE" >&2
    echo "  build it first: make build  (or:  go build -o pincher${EXE_SUFFIX} ./cmd/pinch/)" >&2
    exit 1
fi

if [[ -z "$TARGET" ]]; then
    if ! TARGET="$(command -v "pincher${EXE_SUFFIX}" 2>/dev/null)"; then
        if ! TARGET="$(command -v pincher 2>/dev/null)"; then
            echo "swap-active-binary: no pincher${EXE_SUFFIX} or pincher found in PATH" >&2
            echo "  pass --target=PATH explicitly, or install once via:" >&2
            echo "    cp $SOURCE \$HOME/.local/bin/pincher${EXE_SUFFIX}   # ensure on PATH" >&2
            exit 1
        fi
    fi
fi

if [[ "$(realpath "$SOURCE" 2>/dev/null || echo "$SOURCE")" == "$(realpath "$TARGET" 2>/dev/null || echo "$TARGET")" ]]; then
    echo "swap-active-binary: source and target resolve to the same path — nothing to do"
    exit 0
fi

# Pre-swap safety: refuse to swap a binary that can't even start its MCP
# loop (#710 follow-up). Two probes:
#   1) --version  — checks the binary loads + the version string is stamped
#   2) health-check — full MCP handshake + tools/list call against the
#      $SOURCE binary itself. If this fails, the supervisor's next respawn
#      would crash-loop and the autonomous (AFK) user would come back to
#      a dead MCP. Cost ~1-3s per swap; cheap insurance.
# `health-check` exits 0 = healthy, non-zero = anything wrong. Default
# --timeout is 10s; we cap to 15s to absorb cold-disk variance without
# letting a hung binary stall the loop indefinitely.
# `SKIP_PROBE=1` bypasses both for the rare case where the new binary
# legitimately can't health-check (e.g. early-stage build that hasn't
# wired the MCP loop yet).
if [[ "${SKIP_PROBE:-0}" != "1" ]]; then
    if ! "$SOURCE" --version >/dev/null 2>&1; then
        echo "swap-active-binary: REFUSING swap — $SOURCE failed --version (broken binary?)" >&2
        "$SOURCE" --version 2>&1 | sed 's/^/  /' >&2 || true
        echo "  No swap performed. Existing $TARGET unchanged. Investigate the build before retrying." >&2
        exit 1
    fi
    echo "swap-active-binary: probing MCP handshake on $SOURCE..."
    if ! "$SOURCE" health-check --timeout 15s >/tmp/swap-probe.$$.log 2>&1; then
        echo "swap-active-binary: REFUSING swap — $SOURCE health-check failed (MCP handshake or tools/list broken)" >&2
        echo "  Last 20 lines of probe output:" >&2
        tail -n 20 /tmp/swap-probe.$$.log 2>/dev/null | sed 's/^/  /' >&2 || true
        echo "  No swap performed. Existing $TARGET unchanged. Investigate the build before retrying." >&2
        rm -f /tmp/swap-probe.$$.log 2>/dev/null || true
        exit 1
    fi
    rm -f /tmp/swap-probe.$$.log 2>/dev/null || true
fi

OLD_VERSION="$("$TARGET" --version 2>&1 || echo "(unable to invoke)")"

if [[ "$IS_WINDOWS" == "1" ]]; then
    # Rename-out trick. The running supervisor's open handle survives the
    # rename (Windows tracks inode, not path), so we don't kill the live
    # process. Pre-existing .old gets cleaned up first — its previous
    # tenants have long since exited.
    if [[ -e "${TARGET}.old" ]]; then
        rm -f "${TARGET}.old" 2>/dev/null || true
    fi
    # Only rename if $TARGET exists — fresh installs have nothing to
    # rename out of the way.
    if [[ -e "$TARGET" ]]; then
        mv -f "$TARGET" "${TARGET}.old"
    fi
    cp -f "$SOURCE" "$TARGET"
else
    # POSIX: cp over the live binary is safe because open file handles
    # survive unlink. The supervisor's existing handle continues to point
    # at the inode of the file that was at $TARGET; the new $TARGET file
    # gets a fresh inode.
    cp -f "$SOURCE" "$TARGET"
fi

NEW_VERSION="$("$TARGET" --version 2>&1 || echo "(unable to invoke after swap)")"

echo "swap-active-binary: $TARGET"
echo "  was:  $OLD_VERSION"
echo "  now:  $NEW_VERSION"

if [[ "$OLD_VERSION" == "$NEW_VERSION" ]]; then
    echo "  (no change — source was already the same version)"
fi

cat <<EOF

The supervisor (if running) will detect this swap on its next tool call
and respawn its child onto the new binary, provided
PINCHER_AUTO_RESTART_ON_DRIFT=1 is in its environment. Verify with:

  curl -s \$PINCHER_HTTP_BASE/v1/health 2>/dev/null | grep version
  # or invoke an MCP tool — \`mcp__pincher__health\` shows running version

If auto-restart is off, the next MCP tool call still serves stale code
until the user restarts manually.
EOF
