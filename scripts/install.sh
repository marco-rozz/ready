#!/bin/sh
# ready installer (POSIX sh; runs under sh/bash/zsh including macOS bash 3.2).
#
#   curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | bash
#
# Options cannot be passed to a piped shell directly — use the forms:
#   curl -fsSL <url> | bash -s -- --no-modify-path
#   curl -fsSL <url> | bash -s -- --uninstall
#
# The whole script is one main() invocation on the last line so a truncated
# `curl | bash` executes nothing (every step lives inside main).
#
# Per-user only (per-user-install-agent-isolation): program body at
# $HOME/.local/share/ready/bin/ready published by the relative launcher
# $HOME/.local/bin/ready → ../share/ready/bin/ready; the shipped skill
# library at $HOME/.local/share/ready/library/skills; the default agent
# directory $HOME/.ready is created by ready init. The reference config lives
# in program storage. No prefix,
# global or custom-path installation exists; root execution is refused.
#
# Steps: detect platform (closed table) → select the release URL prefix
# (releases/latest/download or releases/download/$VERSION — no API call, no
# JSON) → download the archive + checksums.txt and verify SHA256 → extract
# and validate bundle members → place (program first: staged on the
# destination filesystem and atomically moved; launcher target-first;
# library rename-aside swap; reference config) → PATH
# (shell-aware, marker-bounded, skipped with --no-modify-path) → final
# message. The installer never touches a runtime skill copy and never stops
# or starts a daemon: an upgrade of an existing agent asks the operator to restart
# `ready serve`.

set -eu

REPO_SLUG="marco-rozz/ready"
ASSET_BASE_DOWNLOAD="https://github.com/${REPO_SLUG}/releases/download"
ASSET_BASE_LATEST="https://github.com/${REPO_SLUG}/releases/latest/download"

BIN_NAME="ready"

main() {
    # ── option parsing (unknown/removed options abort before any side effect) ──
    NO_MODIFY_PATH=0
    DO_UNINSTALL=0
    PURGE=0
    ASSUME_YES=0

    while [ $# -gt 0 ]; do
        case "$1" in
            --no-modify-path)
                NO_MODIFY_PATH=1
                shift
                ;;
            --uninstall)
                DO_UNINSTALL=1
                shift
                ;;
            --purge)
                PURGE=1
                shift
                ;;
            --yes)
                ASSUME_YES=1
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            --prefix|--global|--path)
                die "$1 was removed: READY installs per-user only into \$HOME/.local — see --help"
                ;;
            --*)
                die "unknown option: $1 (supported: --no-modify-path, --uninstall, --purge, --yes)"
                ;;
            *)
                die "unexpected argument: $1"
                ;;
        esac
    done

    [ "$PURGE" = 0 ] || [ "$DO_UNINSTALL" = 1 ] || die "--purge is only meaningful together with --uninstall"

    # Per-user policy: root (uid 0) is refused for EVERY mutating mode —
    # install, uninstall and purge (there is no --global exception).
    if [ "$(id -u)" = 0 ]; then
        die "refusing to run as root: READY installs per-user into your home directory only"
    fi

    if [ "$DO_UNINSTALL" = 1 ]; then
        do_uninstall
        return 0
    fi

    # ── step 1: detect the host platform (closed table) ───────────────────
    detect_platform
    # After this: ASSET_LOCAL (os field of the asset name), ASSET_ARCH,
    # REMOTE_FILE (file name inside the archive), GLIBC_LOADER (loader name
    # for dynamically linked linux assets, empty elsewhere).

    # ── step 2: select the release URL prefix ─────────────────────────────
    URL_PREFIX="$ASSET_BASE_LATEST"
    if [ -n "${VERSION:-}" ]; then
        case "$VERSION" in
            v*) ;;
            *) die "VERSION must be a tag name (v…), got: $VERSION" ;;
        esac
        URL_PREFIX="$ASSET_BASE_DOWNLOAD/$VERSION"
    fi

    # ── step 3: download + SHA256 verify ──────────────────────────────────
    TMP_DIR="$(mktemp -d)"
    # mktemp -d creates a 0700 directory; keep the trap tight.
    trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

    ARCHIVE_NAME="${BIN_NAME}_${ASSET_LOCAL}_${ASSET_ARCH}"
    case "$ASSET_LOCAL" in
        windows) ARCHIVE_EXT=".zip" ;;
        *) ARCHIVE_EXT=".tar.gz" ;;
    esac

    download "$URL_PREFIX/$ARCHIVE_NAME$ARCHIVE_EXT" "$TMP_DIR/$ARCHIVE_NAME$ARCHIVE_EXT"
    download "$URL_PREFIX/checksums.txt" "$TMP_DIR/checksums.txt"
    verify_checksum "$TMP_DIR/$ARCHIVE_NAME$ARCHIVE_EXT" "$TMP_DIR/checksums.txt" "$ARCHIVE_NAME$ARCHIVE_EXT"

    # ── step 4: extract and validate bundle members ───────────────────────
    EXTRACT_DIR="$TMP_DIR/extract"
    mkdir "$EXTRACT_DIR"
    extract "$TMP_DIR/$ARCHIVE_NAME$ARCHIVE_EXT" "$EXTRACT_DIR"
    [ -f "$EXTRACT_DIR/$REMOTE_FILE" ] || die "the archive is missing $REMOTE_FILE (a corrupted or unrelated release?)"
    [ -d "$EXTRACT_DIR/skills" ] || die "the archive is missing the skills library (a corrupted or unrelated release?)"
    [ -f "$EXTRACT_DIR/config.example.yaml" ] || die "the archive is missing config.example.yaml (a corrupted or unrelated release?)"

    # ── step 5: place (program → library → reference config → PATH) ─────────
    USER_HOME="${HOME:-}"
    [ -n "$USER_HOME" ] || die "cannot resolve the user home directory (HOME is empty)"

    SHARE_DIR="$USER_HOME/.local/share/ready"
    BODY_DIR="$SHARE_DIR/bin"
    BODY="$BODY_DIR/$BIN_NAME"
    LAUNCHER_DIR="$USER_HOME/.local/bin"
    LAUNCHER="$LAUNCHER_DIR/$BIN_NAME"
    LIBRARY_DIR="$SHARE_DIR/library/skills"
    AGENT_DIR="$USER_HOME/.ready"

    # Program body: stage in the destination directory (same filesystem) and
    # atomically rename over — the running daemon keeps executing the old
    # inode on unix.
    mkdir -p "$BODY_DIR"
    mv "$EXTRACT_DIR/$REMOTE_FILE" "$BODY_DIR/.$BIN_NAME.new"
    chmod 755 "$BODY_DIR/.$BIN_NAME.new"
    mv -f "$BODY_DIR/.$BIN_NAME.new" "$BODY"

    # Launcher: relative target, created target-first. A foreign occupant at
    # the publication name is refused, never overwritten.
    publish_launcher

    # Library: installer-owned, replaced by a rename-aside swap; runtime
    # skill copies are never touched (the daemon syncs them per file at
    # boot). A failed publication restores the previous library.
    replace_library "$EXTRACT_DIR/skills" "$LIBRARY_DIR"

    # The example is installer-owned program content. Only ready init creates
    # an agent configuration; installation must not pre-empt its first-run wizard.
    cp "$EXTRACT_DIR/config.example.yaml" "$SHARE_DIR/.config.example.yaml.new"
    chmod 644 "$SHARE_DIR/.config.example.yaml.new"
    mv -f "$SHARE_DIR/.config.example.yaml.new" "$SHARE_DIR/config.example.yaml"

    # ── step 6: PATH ──────────────────────────────────────────────────────
    if [ "$NO_MODIFY_PATH" = 1 ]; then
        PATH_OUTCOME="skipped (--no-modify-path)"
        print_manual_path
    else
        handle_path
    fi

    # ── step 7: final message ─────────────────────────────────────────────
    printf 'Installed %s\n' "$(installed_version)"
    printf 'program: %s (launcher: %s)\n' "$BODY" "$LAUNCHER"
    printf 'shipped skill library: %s (installer-owned; each agent keeps its own runtime copy)\n' "$LIBRARY_DIR"
    printf 'reference configuration: %s/config.example.yaml\n' "$SHARE_DIR"
    printf 'PATH: %s\n' "$PATH_OUTCOME"
    if [ -f "$AGENT_DIR/config.yaml" ]; then
        printf 'kept the existing config at %s/config.yaml\n' "$AGENT_DIR"
        printf 'For offline configuration changes, run: ready init\n'
        printf 'restart ready serve to use the new version\n'
    else
        printf 'No agent configuration was created by the installer.\n'
        printf 'Next: open a new terminal (apply the PATH instructions above if needed), then run:\n  ready init\n'
        printf 'The wizard will create %s/config.yaml and print your server and chat commands.\n' "$AGENT_DIR"
    fi
}

usage() {
    cat <<'EOF'
ready installer

Options:
  --no-modify-path  do not edit the shell's startup PATH (manual
                    instructions are printed either way)
  --uninstall       remove the program and the shipped skill library
                    (PATH and agent data are never touched)
  --purge           together with --uninstall: also delete the default
                    agent directory $HOME/.ready (config, transcripts,
                    OAuth credentials, runtime skills) — asks for
                    confirmation unless --yes is given; refuses an active
                    daemon/login; explicit --agent-dir directories are
                    never touched
  --yes             do not ask for confirmation

Environment:
  VERSION=v1.2.3    pin the release tag instead of the latest stable
EOF
}

die() {
    printf 'ready installer: %s\n' "$1" >&2
    exit 1
}

# ── platform detection (closed table, D15) ──────────────────────────────────
detect_platform() {
    OS="$(uname -s)"
    ARCH_UNAME="$(uname -m)"

    case "$OS" in
        Linux)
            ASSET_LOCAL="linux"
            case "$ARCH_UNAME" in
                x86_64)
                    ASSET_ARCH="amd64"
                    GLIBC_LOADER="/lib64/ld-linux-x86-64.so.2"
                    ;;
                aarch64|arm64)
                    ASSET_ARCH="arm64"
                    GLIBC_LOADER="/lib/ld-linux-aarch64.so.1"
                    ;;
                # an arm64 kernel reports armv8l to 32-bit processes: a
                # 32-bit userland on a 64-bit kernel lands here.
                armv6l|armv7l|armv8l)
                    ASSET_ARCH="armv6"
                    GLIBC_LOADER=""
                    ;;
                *)
                    die "unsupported linux architecture \"$ARCH_UNAME\" — supported: amd64 (x86_64), arm64 (aarch64), arm/v6 (armv6l, armv7l, armv8l)"
                    ;;
            esac
            REMOTE_FILE="$BIN_NAME"
            # linux amd64/arm64 binaries are dynamically linked (the desktop
            # TUI keeps the SHIFT+RETURN probe): require the glibc loader
            # before any write, naming the missing loader.
            if [ -n "$GLIBC_LOADER" ] && [ ! -e "$GLIBC_LOADER" ]; then
                die "your $ASSET_ARCH/linux system lacks the glibc dynamic loader ($GLIBC_LOADER) — Alpine/musl, NixOS and distroless hosts are unsupported (no static build for $ASSET_ARCH today)"
            fi
            ;;
        Darwin)
            ASSET_LOCAL="darwin"
            GLIBC_LOADER=""
            REMOTE_FILE="$BIN_NAME"
            # uname -m lies under Rosetta: ask the hardware, not the shell.
            # Absolute path first: a reduced PATH (cron, launchd, env -i)
            # may lack /usr/sbin and would silently select amd64.
            if [ "$(/usr/sbin/sysctl -n hw.optional.arm64 2>/dev/null || sysctl -n hw.optional.arm64 2>/dev/null || true)" = "1" ]; then
                ASSET_ARCH="arm64"
            else
                ASSET_ARCH="amd64"
            fi
            ;;
        *)
            die "unsupported operating system \"$OS\" — supported: Linux (amd64, arm64, arm/v6) and macOS (amd64, arm64)"
            ;;
    esac
}

installed_version() {
    "$BODY" --version 2>/dev/null || printf '%s (version check unavailable)' "$BIN_NAME"
}

# ── download (curl preferred, wget fallback; TLS pinned) ────────────────────
download() {
    url="$1"
    dst="$2"
    if command -v curl >/dev/null 2>&1; then
        # TLS pinned: a downgrade to http would let any middle do the rest.
        curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error --output "$dst" "$url" \
            || die "download failed: $url"
    elif command -v wget >/dev/null 2>&1; then
        wget --secure-protocol=TLSv1_2 -qO "$dst" "$url" || die "download failed: $url"
    else
        die "neither curl nor wget is available to fetch $url"
    fi
}

verify_checksum() {
    archive="$1"
    checksums="$2"
    asset="$3"

    want="$(awk -v file="$asset" '$2 == file { print $1; exit }' "$checksums")"
    [ -n "$want" ] || die "checksums.txt does not list $asset"

    if command -v sha256sum >/dev/null 2>&1; then
        got="$(sha256sum "$archive" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        got="$(shasum -a 256 "$archive" | awk '{print $1}')"
    else
        die "neither sha256sum nor shasum is available to verify the download"
    fi

    if [ "$got" != "$want" ]; then
        die "SHA256 mismatch for $asset (want $want, got $got) — the release may have changed between the two downloads; retry"
    fi
}

extract() {
    archive="$1"
    dst="$2"
    case "$archive" in
        *.tar.gz|*.tgz|*.tar)
            tar -xzf "$archive" -C "$dst"
            ;;
        *)
            die "unknown archive type: $archive"
            ;;
    esac
}

# ── launcher publication (target first, foreign occupants refused) ──────────
# The launcher is a relative symlink ../share/ready/bin/ready living in
# $HOME/.local/bin. Publication is atomic (tmp symlink + mv -f) and refuses
# a foreign occupant at the name.
publish_launcher() {
    mkdir -p "$LAUNCHER_DIR"

    if [ -L "$LAUNCHER" ]; then
        # Ours or dangling ours: recreate target-first (a prior body move may
        # have left the link dangling).
        current_target="$(readlink "$LAUNCHER" || true)"
        if [ "$current_target" != "../share/ready/bin/ready" ]; then
            die "refusing to replace $LAUNCHER (it is a symlink to \"$current_target\", not READY's launcher)"
        fi
    elif [ -e "$LAUNCHER" ]; then
        die "refusing to replace $LAUNCHER (a non-READY file, link or directory occupies the launcher name — move it away and retry)"
    fi

    rm -f "$LAUNCHER_DIR/.$BIN_NAME.new-launch"
    ln -s "../share/ready/bin/ready" "$LAUNCHER_DIR/.$BIN_NAME.new-launch"
    mv -f "$LAUNCHER_DIR/.$BIN_NAME.new-launch" "$LAUNCHER"
}

# ── library replacement (installer-owned, rename-aside swap) ────────────────
replace_library() {
    src="$1"
    dst="$2"

    mkdir -p "$(dirname "$dst")"
    if [ -d "$dst" ]; then
        rm -rf "${dst}.backup" 2>/dev/null || true
        mv "$dst" "${dst}.backup"
    fi
    if mv "$src" "$dst"; then
        rm -rf "${dst}.backup" 2>/dev/null || true
    else
        # Publication failed: restore the previous library.
        if [ -d "${dst}.backup" ]; then
            mv "${dst}.backup" "$dst"
        fi
        die "failed to publish the skill library at $dst"
    fi
}

# ── maintenance gate (installer↔daemon purge coordination) ──────────────────
# start_gate runs the freshly-placed binary as the EXCLUSIVE maintenance-gate
# holder for the agent directory and waits (bounded) for its readiness
# marker. stdout carries the holder's output file path via HOLD_OUT; the PID
# via HOLD_PID. Refusal (busy daemon/login) aborts BEFORE any deletion.
start_gate() {
    gate_dir="$1"
    TMP_DIR="${TMP_DIR:-$(mktemp -d)}"
    hold_out="$TMP_DIR/maintenance.out"
    rm -f "$hold_out"
    "$BODY" maintenance --agent-dir "$gate_dir" >"$hold_out" 2>&1 &
    HOLD_PID=$!

    i=0
    while [ "$i" -lt 100 ]; do
        if grep -q "MAINTENANCE-LOCKED" "$hold_out" 2>/dev/null; then
            return 0
        fi
        if ! kill -0 "$HOLD_PID" 2>/dev/null; then
            cat "$hold_out" >&2 || true
            die "cannot take the exclusive maintenance gate on $gate_dir — a daemon or mutating login appears active; stop it and retry"
        fi
        sleep 0.1
        i=$((i + 1))
    done
    kill "$HOLD_PID" 2>/dev/null || true
    die "the maintenance gate on $gate_dir did not become available in time; retry"
}

stop_gate() {
    if [ -n "${HOLD_PID:-}" ]; then
        kill "$HOLD_PID" 2>/dev/null || true
        wait "$HOLD_PID" 2>/dev/null || true
        HOLD_PID=""
    fi
}

# ── PATH coverage (shell-aware, marker-bounded, opt-out) ────────────────────
block_begin="# >>> ready >>>"
block_end="# <<< ready <<<"

print_manual_path() {
    printf 'manual PATH (always valid): add this to your shell startup file:\n  export PATH="%s:$PATH"\n' "$LAUNCHER_DIR"
}

handle_path() {
    # The SELECTED user shell — not the shell executing this piped script.
    user_shell="${SHELL:-}"
    if [ -z "$user_shell" ]; then
        PATH_OUTCOME="unknown (no login shell detected)"
        print_manual_path

        return
    fi
    case "$user_shell" in
        */zsh) rc="$HOME/.zshrc" ;;
        */bash) rc="$HOME/.bashrc" ;;
        *) rc="$HOME/.profile" ;;
    esac

    # 1. Current reachability: the launcher directory in the effective PATH.
    case ":$PATH:" in
        *:"$LAUNCHER_DIR":*)
            PATH_OUTCOME="already reachable in the current PATH"
            print_manual_path

            return
            ;;
    esac

    # 2. Configured for the SELECTED shell: an existing READY block in its rc
    # (unrelated rc files, comments and inactive conditionals are never
    # proof — only the selected rc's own bytes count).
    if [ -f "$rc" ] && grep -qF "$block_begin" "$rc"; then
        PATH_OUTCOME="configured (existing READY block in $(basename "$rc"))"
        print_manual_path

        return
    fi

    # 3. Edit at most one applicable rc, in a marker-bounded block.
    if [ -f "$rc" ] || [ -w "$HOME" ]; then
        mkdir -p "$(dirname "$rc")"
        touch "$rc"
        {
            printf '\n'
            printf '%s\n' "$block_begin"
            printf 'export PATH="%s:$PATH"\n' "$LAUNCHER_DIR"
            printf '%s\n' "$block_end"
        } >>"$rc"
        PATH_OUTCOME="edited $(basename "$rc") (starts a new shell to apply)"
        print_manual_path

        return
    fi

    PATH_OUTCOME="unknown (could not edit $(basename "$rc"))"
    print_manual_path
}

# ── uninstall ───────────────────────────────────────────────────────────────
# Removes only owned program/launcher/library entries. Shared PATH entries
# and ALL agent state (default and explicit directories) are never touched.
do_uninstall() {
    USER_HOME="${HOME:-}"
    [ -n "$USER_HOME" ] || die "cannot resolve the user home directory (HOME is empty)"

    SHARE_DIR="$USER_HOME/.local/share/ready"
    BODY="$SHARE_DIR/bin/$BIN_NAME"
    LAUNCHER="$USER_HOME/.local/bin/$BIN_NAME"
    LIBRARY_DIR="$SHARE_DIR/library/skills"
    AGENT_DIR="$USER_HOME/.ready"

    if [ "$PURGE" = 1 ]; then
        do_purge
        return
    fi

    if [ -f "$BODY" ]; then
        rm -f "$BODY"
        printf 'removed the program %s\n' "$BODY"
    else
        printf 'no program found at %s\n' "$BODY"
    fi

    if [ -L "$LAUNCHER" ]; then
        rm -f "$LAUNCHER"
        printf 'removed the launcher %s\n' "$LAUNCHER"
    elif [ -e "$LAUNCHER" ]; then
        die "refusing to remove $LAUNCHER (a non-READY file occupies the launcher name)"
    else
        printf 'no launcher found at %s\n' "$LAUNCHER"
    fi

    if [ -d "$SHARE_DIR" ]; then
        rm -rf "$SHARE_DIR"
        printf 'removed the shipped skill library %s\n' "$LIBRARY_DIR"
    else
        printf 'no shipped skill library found at %s\n' "$LIBRARY_DIR"
    fi

    printf 'agent data kept: %s holds your config, session transcripts, OAuth credentials and runtime skills\n' "$AGENT_DIR"
    printf 'explicit --agent-dir directories are never discovered or removed\n'
    printf 'PATH entries were never touched\n'
    printf 'uninstalled.\n'
}

# ── combined purge (uninstall --purge) ──────────────────────────────────────
# Confirms, takes the EXCLUSIVE maintenance gate on the default agent
# directory, then deletes — config first so no new daemon can boot against a
# recreated lock with an obsolete snapshot. Refuses: a busy daemon/login
# (shared gate), a linked default agent directory, and any explicit agent
# directory (never discovered).
do_purge() {
    if [ -L "$AGENT_DIR" ]; then
        die "refusing to purge $AGENT_DIR: it is a symbolic link (the real directory it points to is never traversed for deletion)"
    fi
    [ -d "$AGENT_DIR" ] || die "no default agent directory at $AGENT_DIR — nothing to purge"

    if [ "$ASSUME_YES" = 0 ]; then
        # Ask on the terminal (not the pipe): a piped installer may have
        # no stdin at all.
        if [ -e /dev/tty ]; then
            printf 'about to delete %s — this removes config, session transcripts, OAuth credentials and runtime skills permanently. type "yes" to confirm: ' "$AGENT_DIR" >/dev/tty
            read -r answer </dev/tty
        else
            die "purge would delete $AGENT_DIR (config, transcripts, OAuth credentials, runtime skills) but no terminal is available for confirmation — run with --yes to proceed non-interactively"
        fi
        [ "$answer" = "yes" ] || die "purge aborted (nothing deleted)"
    fi

    # Exclusive maintenance gate BEFORE any deletion (including the program
    # and library below): a busy daemon/login aborts everything untouched.
    start_gate "$AGENT_DIR"

    # Forget the Store key's vault entry (macOS Keychain) BEFORE the agent
    # directory and the program disappear: it names the agent id kept in the
    # config. Best-effort — a key held in the agent directory goes away with it.
    if [ -x "$BODY" ]; then
        if ! "$BODY" secrets forget-key --agent-dir "$AGENT_DIR" --yes >"$TMP_DIR/forget.out" 2>&1; then
            printf 'note: the store key could not be forgotten automatically (the agent may predate the secret store, or its config is unreadable).\n' >&2
            printf '      If it kept its key in the OS vault, remove the "ready" entry named after the agent id by hand.\n' >&2
        fi
    fi

    # Config first: a new daemon cannot boot without it even if it raced a
    # recreated gate file (its ownership recheck refuses a missing config).
    rm -f "$AGENT_DIR/config.yaml"
    rm -rf "$AGENT_DIR"
    printf 'deleted the default agent directory %s\n' "$AGENT_DIR"

    # Program and library (owned entries only; the gate stays held).
    if [ -f "$BODY" ]; then
        rm -f "$BODY"
        printf 'removed the program %s\n' "$BODY"
    fi
    if [ -L "$LAUNCHER" ]; then
        rm -f "$LAUNCHER"
        printf 'removed the launcher %s\n' "$LAUNCHER"
    elif [ -e "$LAUNCHER" ]; then
        printf 'kept %s (a non-READY file occupies the launcher name)\n' "$LAUNCHER" >&2
    fi
    if [ -d "$SHARE_DIR" ]; then
        rm -rf "$SHARE_DIR"
        printf 'removed the shipped skill library %s\n' "$LIBRARY_DIR"
    fi

    stop_gate

    printf 'explicit agent directories and PATH entries remain untouched\n'
    printf 'uninstalled.\n'
}

main "$@"
