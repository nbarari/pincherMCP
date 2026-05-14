#!/usr/bin/env bash
# build.sh — Windows-safe `go build` wrapper for the pincher binary (#710).
#
# Why this exists: on Windows the OS holds an exclusive file lock on
# every running .exe. If `./pincher.exe` was previously launched by
# `pincher web` or a direct `--http` invocation, `go build -o pincher.exe`
# fails with "open pincher.exe: The process cannot access the file
# because it is being used by another process." #708 fixed the swap step
# but the source-side build itself was still subject to the lock.
#
# This wrapper always builds to a side name (`pincher.new.exe` /
# `pincher.new`) then renames over the target. Windows allows renaming
# over a locked file because handle resolution is by inode, not path —
# the running process keeps using the old inode.
#
# Usage:
#   bash scripts/build.sh                              # default target ./pincher{,.exe}
#   bash scripts/build.sh --bin=path/to/binary         # override target
#   bash scripts/build.sh -- -ldflags="-X foo=bar"     # extra args after -- forwarded to go build
#
# Version stamping (matches Makefile build target):
#   PINCHER_VERSION env wins; otherwise `git describe --tags --dirty --always`.

set -euo pipefail

case "$(uname -s 2>/dev/null || echo Windows)" in
    MINGW*|CYGWIN*|MSYS*|Windows*) EXE_SUFFIX=.exe ;;
    *)                             EXE_SUFFIX= ;;
esac

OUT="./pincher${EXE_SUFFIX}"
EXTRA_ARGS=()
parse_extras=0

for arg in "$@"; do
    if [[ "$parse_extras" == "1" ]]; then
        EXTRA_ARGS+=("$arg")
        continue
    fi
    case "$arg" in
        --bin=*) OUT="${arg#--bin=}" ;;
        --)      parse_extras=1 ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "build: unknown arg: $arg" >&2
            exit 2
            ;;
    esac
done

VERSION="${PINCHER_VERSION:-}"
if [[ -z "$VERSION" ]]; then
    VERSION="$(git describe --tags --dirty --always 2>/dev/null | sed 's/^v//' || echo dev)"
fi

# Side-name build, then rename. The `.new` suffix avoids colliding with
# any in-flight process holding the canonical $OUT path open.
SIDE="${OUT}.new"
rm -f "$SIDE" 2>/dev/null || true

LDFLAGS="-s -w -X main.version=$VERSION"
# macOS CI runners ship bash 3.2, where "${EMPTY_ARRAY[@]}" under `set -u`
# throws "unbound variable". Guard the expansion: only splice EXTRA_ARGS
# when it's non-empty. (bash 4.4+ handles the bare form fine, but we
# can't assume that on the macOS test job.)
if [[ ${#EXTRA_ARGS[@]} -gt 0 ]]; then
    go build -trimpath -ldflags="$LDFLAGS" -o "$SIDE" "${EXTRA_ARGS[@]}" ./cmd/pinch/
else
    go build -trimpath -ldflags="$LDFLAGS" -o "$SIDE" ./cmd/pinch/
fi

# Now move the new build over the target. Rename works on Windows even
# when $OUT is locked (open handles follow inode). On POSIX, plain mv
# does the right thing too — open handles survive replace.
if [[ -e "$OUT" ]]; then
    # Stash the previous binary so the move never blocks. The .old file
    # gets cleaned up after a successful move-in.
    rm -f "${OUT}.old" 2>/dev/null || true
    mv -f "$OUT" "${OUT}.old" || {
        # Fallback: if the move failed (rare), the rename of $SIDE → $OUT
        # below will still succeed on Windows for the same handle-inode
        # reason. Keep going.
        :
    }
fi
mv -f "$SIDE" "$OUT"
rm -f "${OUT}.old" 2>/dev/null || true

echo "build: $("$OUT" --version 2>&1) → $OUT"
