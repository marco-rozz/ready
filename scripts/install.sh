#!/bin/sh
# ready installer (POSIX sh; runs under sh/bash/zsh including macOS bash 3.2).
#
#   curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | bash
#
# Options cannot be passed to a piped shell directly — use the forms:
#   curl -fsSL <url> | bash -s -- --path           (apply the PATH export)
#   curl -fsSL <url> | bash -s -- --uninstall
#
# The whole script is one main() invocation on the last line so a truncated
# `curl | bash` executes nothing (every step lives inside main).
#
# Steps: detect platform (closed table) → select the release URL prefix
# (releases/latest/download or releases/download/$VERSION — no API call, no
# JSON) → download the archive + checksums.txt and verify SHA256 → extract to
# a mktemp dir → place (binary first: staged as .ready.new and atomically
# moved; config copy-if-missing 0600; library replaced by a rename-aside
# swap) → PATH (print-only by default; --path edits the rc file, idempotently)
# → final message. The installer never touches a runtime skill copy and never
# stops or starts a daemon: every upgrade ends by asking the operator to
# restart `ready serve`.

set -eu

REPO_SLUG="marco-rozz/ready"
ASSET_BASE_DOWNLOAD="https://github.com/${REPO_SLUG}/releases/download"
ASSET_BASE_LATEST="https://github.com/${REPO_SLUG}/releases/latest/download"
RAW_BASE="https://raw.githubusercontent.com/${REPO_SLUG}/main"

BIN_NAME="ready"
UNINSTALL_BIN_NAME="uninstall"

main() {
    # ── option parsing (unknown options abort before any side effect) ─────
    PREFIX_OPT=""
    GLOBAL=0
    EDIT_PATH=0
    DO_UNINSTALL=0
    PURGE=0
    ASSUME_YES=0

    while [ $# -gt 0 ]; do
        case "$1" in
            --prefix)
                [ $# -ge 2 ] || die "--prefix requires a directory argument"
                PREFIX_OPT="$2"
                shift 2
                ;;
            --global)
                GLOBAL=1
                shift
                ;;
            --path)
                EDIT_PATH=1
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
            --*)
                die "unknown option: $1 (supported: --prefix <dir>, --global, --path, --uninstall, --purge, --yes)"
                ;;
            *)
                die "unexpected argument: $1"
                ;;
        esac
    done

    [ "$GLOBAL" = 1 ] || [ -z "$PREFIX_OPT" ] || die "--prefix and --global are mutually exclusive"
    [ "$PURGE" = 0 ] || [ "$DO_UNINSTALL" = 1 ] || die "--purge is only meaningful together with --uninstall"

    if [ "$DO_UNINSTALL" = 1 ]; then
        do_uninstall
        return 0
    fi

    # Running as root without --global would leave a root-owned ~/.ready in
    # the user's home — abort before any write.
    if [ "$(id -u)" = 0 ] && [ "$GLOBAL" != 1 ]; then
        die "refusing to run as root without --global (it would create a root-owned install home in the user's home directory)"
    fi

    # ── step 1: detect the host platform (closed table) ───────────────────
    detect_platform
    # After this: ASSET_LOCAL (os field of the asset name), ASSET_ARCH,
    # REMOTE_FILE (file name inside the archive), GLIBC_LOADER (loader name
    # for dynamically linked linux assets, empty elsewhere).

    # ── step 2: select the release URL prefix ─────────────────────────────
    URL_PREFIX="$ASSET_BASE_LATEST"
    ASSET_VERSION_SUFFIX=""
    if [ -n "${VERSION:-}" ]; then
        case "$VERSION" in
            v*) VERSION_TAG="$VERSION" ;;
            *) die "VERSION must be a tag name (v…), got: $VERSION" ;;
        esac
        URL_PREFIX="$ASSET_BASE_DOWNLOAD/$VERSION"
        ASSET_VERSION_SUFFIX="/$VERSION_TAG/"
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

    # ── step 4: extract ───────────────────────────────────────────────────
    EXTRACT_DIR="$TMP_DIR/extract"
    mkdir "$EXTRACT_DIR"
    extract "$TMP_DIR/$ARCHIVE_NAME$ARCHIVE_EXT" "$EXTRACT_DIR"
    [ -f "$EXTRACT_DIR/$REMOTE_FILE" ] || die "the archive is missing $REMOTE_FILE (a corrupted or unrelated release?)"

    # ── step 5: place (binary first — a later step failing leaves the earlier ones) ──
    INSTALL_HOME="$(install_home)"
    BIN_DIR="$(bin_dir)"

    mkdir -p "$BIN_DIR"
    if [ "$GLOBAL" = 1 ]; then
        sudo install -m 0755 "$EXTRACT_DIR/$REMOTE_FILE" "$BIN_DIR/.$BIN_NAME.new"
        sudo mv -f "$BIN_DIR/.$BIN_NAME.new" "$BIN_DIR/$BIN_NAME"
    else
        # Stage in the same directory (same filesystem), chmod, atomic
        # rename: the running daemon keeps executing the old inode on unix.
        mv "$EXTRACT_DIR/$REMOTE_FILE" "$BIN_DIR/.$BIN_NAME.new"
        chmod 755 "$BIN_DIR/.$BIN_NAME.new"
        mv -f "$BIN_DIR/.$BIN_NAME.new" "$BIN_DIR/$BIN_NAME"
    fi

    # The installed config: copy-if-missing, never replacing the operator's.
    if [ ! -f "$INSTALL_HOME/config.yaml" ]; then
        mkdir -p "$INSTALL_HOME"
        chmod 700 "$INSTALL_HOME" 2>/dev/null || true
        cp "$EXTRACT_DIR/config.example.yaml" "$INSTALL_HOME/config.yaml"
        chmod 600 "$INSTALL_HOME/config.yaml"
        KEPT_CONFIG=0
    else
        KEPT_CONFIG=1
    fi

    replace_library "$EXTRACT_DIR/skills" "$INSTALL_HOME/library/skills"

    # ── step 6: PATH ──────────────────────────────────────────────────────
    handle_path

    # ── step 7: final message ─────────────────────────────────────────────
    printf 'Installed %s\n' "$(installed_version)"
    if [ "$KEPT_CONFIG" = 1 ]; then
        printf 'kept the existing config at %s/config.yaml — edit it if needed\n' "$INSTALL_HOME"
    else
        printf 'created %s/config.yaml — edit it (provider endpoints, tokens) before first use\n' "$INSTALL_HOME"
    fi
    printf 'shipped skill library at %s/library/skills (replaced on every install; the runtime copy at %s/skills is synced per file by the daemon)\n' "$INSTALL_HOME" "$INSTALL_HOME"
    printf 'restart ready serve to use the new version\n'
}

usage() {
    cat <<'EOF'
ready installer

Options:
  --prefix <dir>  install the binary into <dir> (default: $HOME/.ready/bin)
  --global        install into /usr/local/bin using sudo (mutually exclusive
                  with --prefix)
  --path          add the bin directory to your shell's rc file (default is
                  print-only)
  --uninstall     remove the binary (and the PATH block it added)
  --purge         together with --uninstall: also delete the install home
                  (config, transcripts, credentials, skills) — asks for
                  confirmation unless --yes is given
  --yes           do not ask for confirmation

Environment:
  VERSION=v1.2.3  pin the release tag instead of the latest stable
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
            if [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = "1" ]; then
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

install_home() {
    USER_HOME="${HOME:-}"
    [ -n "$USER_HOME" ] || die "cannot resolve the user home directory (HOME is empty)"
    printf '%s/.ready' "$USER_HOME"
}

bin_dir() {
    if [ "$GLOBAL" = 1 ]; then
        printf '/usr/local/bin'
    elif [ -n "$PREFIX_OPT" ]; then
        printf '%s' "$PREFIX_OPT"
    else
        printf '%s/bin' "$(install_home)"
    fi
}

installed_version() {
    "$BIN_DIR/$BIN_NAME" --version 2>/dev/null || printf '%s (version check unavailable)' "$BIN_NAME"
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
        *.zip)
            if command -v unzip >/dev/null 2>&1; then
                unzip -qo "$archive" -d "$dst" || die "could not extract the release archive"
            else
                die "unzip is not available to extract a windows archive"
            fi
            ;;
        *)
            die "unknown archive type: $archive"
            ;;
    esac
}

# ── library replacement (installer-owned, replaced wholesale) ───────────────
# The swap is pure renames: the microsecond window in which library/skills is
# absent is harmless — the daemon reads the library only as the
# injected-skill fallback. No hashing in the script; the daemon's boot sync
# reconciles every runtime copy per file.
replace_library() {
    src="$1"
    dst="$2"

    [ -d "$src" ] || die "the release archive carries no skills library"

    mkdir -p "$(dirname "$dst")"
    if [ -d "$dst" ]; then
        rm -rf "${dst}.backup" 2>/dev/null || true
        mv "$dst" "${dst}.backup"
    fi
    mv "$src" "$dst"
    rm -rf "${dst}.backup" 2>/dev/null || true
}

# ── PATH coverage ───────────────────────────────────────────────────────────
# Default: print-only (a piped `curl | bash` that silently edits rc files is
# not READY's style). --path opts into a detected rc file, idempotently.
handle_path() {
    bin_path="$(bin_dir)"

    block_begin="# >>> ready install >>>"
    block_end="# <<< ready install <<<"
    block="$block_begin
export PATH=\"$bin_path:\$PATH\"
$block_end"

    printf 'add %s to your PATH:\n  export PATH="%s:$PATH"\n\n' "$bin_path" "$bin_path"

    [ "$EDIT_PATH" = 1 ] || return 0

    rc="$(rc_file)"
    [ -n "$rc" ] || die "could not detect a shell rc file to edit — add the export line above by hand, or pass --prefix with an rc-managed directory"

    mkdir -p "$(dirname "$rc")"
    touch "$rc"
    if grep -qF "$block_begin" "$rc"; then
        printf 'PATH block already present in %s (idempotent — not added twice)\n' "$rc"
        return 0
    fi

    {
        printf '\n'
        printf '%s\n' "$block"
    } >>"$rc"
    printf 'added the PATH block to %s (starts a new shell to apply)\n' "$rc"

    UNINSTALL_RC="$rc"
}

rc_file() {
    if [ -n "${ZSH_VERSION:-}" ] && [ -f "$HOME/.zshrc" ]; then
        printf '%s/.zshrc' "$HOME"
    elif [ -f "$HOME/.bashrc" ]; then
        printf '%s/.bashrc' "$HOME"
    elif [ -f "$HOME/.profile" ]; then
        printf '%s/.profile' "$HOME"
    else
        printf '%s/.profile' "$HOME"
    fi
}

# ── uninstall ───────────────────────────────────────────────────────────────
do_uninstall() {
    bin_path="$(bin_dir)"
    bin_file="$bin_path/$BIN_NAME"
    install_root="$(install_home)"

    if [ -f "$bin_file" ]; then
        rm -f "$bin_file"
        printf 'removed the binary from %s\n' "$bin_path"
    else
        printf 'no binary found at %s\n' "$bin_file"
    fi
    if [ -n "${PREFIX_OPT:-}${UNINSTALL_RC:-}" ]; then
        :
    fi

    # Remove the PATH block from every rc file that may carry it.
    for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile"; do
        [ -f "$rc" ] || continue
        if grep -qF "# >>> ready install >>>" "$rc"; then
            awk 'BEGIN{skip=0} /# >>> ready install >>>/{skip=1; next} /# <<< ready install <<</{skip=0; next} skip==0{print}' "$rc" >"$rc.tmp"
            mv "$rc.tmp" "$rc"
            printf 'removed the PATH block from %s\n' "$rc"
        fi
    done

    if [ "$PURGE" = 1 ]; then
        if [ "$ASSUME_YES" = 0 ]; then
            # Ask on the terminal (not the pipe): a piped installer may have
            # no stdin at all.
            if [ -e /dev/tty ]; then
                printf 'about to delete %s — this removes config, session transcripts, OAuth credentials and skills permanently. type "yes" to confirm: ' "$install_root" >/dev/tty
                read -r answer </dev/tty
            else
                die "purge would delete $install_root (config, transcripts, OAuth credentials, skills) but no terminal is available for confirmation — run with --yes to proceed non-interactively"
            fi
            [ "$answer" = "yes" ] || die "purge aborted (nothing deleted)"
        fi
        rm -rf "$install_root"
        printf 'deleted %s\n' "$install_root"
    else
        printf 'data kept: %s holds your config, session transcripts, OAuth credentials and skills\n' "$install_root"
        printf 'remove it by hand if that is what you want (or uninstall with --purge)\n'
    fi
    printf 'uninstalled.\n'
}

main "$@"
