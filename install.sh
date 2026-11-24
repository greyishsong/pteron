#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
DIST_DIR=$SCRIPT_DIR/dist
BIN_DIR=${BIN_DIR:-$HOME/.local/bin}
SERVICE_DIR=${SERVICE_DIR:-$HOME/.config/systemd/user}
SKILL_DIR=${SKILL_DIR:-$HOME/.gemini/config/skills}

usage() {
    cat <<'EOF'
Usage: ./install.sh [all|build|package|install|help] [options]

  all              Build, package, and install (default in a source checkout).
  build            Compile the binary to dist/pteron.
  package          Build and create dist/pteron-<os>-<arch>.tar.gz.
  install          Install for this host (default in a release bundle).

  --bin-dir DIR      Binary directory (BIN_DIR; default: ~/.local/bin).
  --service-dir DIR  User unit directory (SERVICE_DIR; default: ~/.config/systemd/user).
  --no-service       Skip the systemd user service entirely.
  --no-restart       Install the unit without enabling or restarting the service.
  --with-skill       Also install the pteron skill.
  --skill-dir DIR    Skill root (SKILL_DIR; default: ~/.gemini/config/skills); implies --with-skill.
  --os GOOS          Build target OS (defaults to GOOS or `go env GOOS`).
  --arch GOARCH      Build target architecture (defaults to GOARCH or `go env GOARCH`).
  -h, --help         Show this help.
EOF
}

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

command_name= os_flag= arch_flag=
service=true restart=true skill=false
while (($#)); do
    case $1 in
        -h|--help|help) usage; exit 0 ;;
        all|build|package|install)
            [[ -z $command_name ]] || die "choose only one subcommand"
            command_name=$1 ;;
        --no-service) service=false ;;
        --no-restart) restart=false ;;
        --with-skill) skill=true ;;
        --bin-dir|--service-dir|--skill-dir|--os|--arch)
            [[ -n ${2:-} && $2 != -* ]] || die "$1 requires a value"
            case $1 in
                --bin-dir) BIN_DIR=$2 ;;
                --service-dir) SERVICE_DIR=$2 ;;
                --skill-dir) SKILL_DIR=$2; skill=true ;;
                --os) os_flag=$2 ;;
                --arch) arch_flag=$2 ;;
            esac
            shift ;;
        -*) die "unknown option: $1 (see --help)" ;;
        *) die "unknown subcommand: $1 (see --help)" ;;
    esac
    shift
done

source_checkout=false
if [[ -f $SCRIPT_DIR/go.mod ]]; then
    source_checkout=true
    command_name=${command_name:-all}
elif [[ -f $SCRIPT_DIR/pteron ]]; then
    command_name=${command_name:-install}
    [[ $command_name == install ]] || die "$command_name requires a source checkout"
else
    die "expected a source checkout or an unpacked release bundle"
fi

build() (
    cd -- "$SCRIPT_DIR"
    mkdir -p -- "$DIST_DIR"
    CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build -trimpath -ldflags="-s -w" -o "$DIST_DIR/pteron" .
    printf 'Built %s (%s/%s)\n' "$DIST_DIR/pteron" "$TARGET_OS" "$TARGET_ARCH"
)

package() (
    local name="pteron-$TARGET_OS-$TARGET_ARCH" stage root
    stage=$(mktemp -d "$DIST_DIR/.package.XXXXXXXX")
    trap 'rm -rf -- "$stage"' EXIT
    trap 'exit 1' HUP INT TERM
    root=$stage/$name
    mkdir -p -- "$root/skills/pteron"
    install -m 0755 "$DIST_DIR/pteron" "$root/pteron"
    install -m 0755 "$SCRIPT_DIR/install.sh" "$root/install.sh"
    install -m 0644 "$SCRIPT_DIR/pteron.service" "$SCRIPT_DIR/README.md" "$root/"
    install -m 0644 "$SCRIPT_DIR/skills/pteron/SKILL.md" "$root/skills/pteron/SKILL.md"
    tar -czf "$stage/$name.tar.gz" -C "$stage" "$name"
    mv -f -- "$stage/$name.tar.gz" "$DIST_DIR/$name.tar.gz"
    printf 'Packaged %s\n' "$DIST_DIR/$name.tar.gz"
)

# With no Go toolchain in a bundle, use uname for explicit target checks.
bundle_host() {
    case $(uname -s) in
        MINGW*|MSYS*|CYGWIN*) HOST_OS=windows ;;
        SunOS) HOST_OS=solaris ;;
        *) HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]') ;;
    esac
    case $(uname -m) in
        x86_64|amd64) HOST_ARCH=amd64 ;;
        i?86) HOST_ARCH=386 ;;
        aarch64|arm64) HOST_ARCH=arm64 ;;
        arm*) HOST_ARCH=arm ;;
        *) HOST_ARCH=$(uname -m) ;;
    esac
}

matches_host() {
    go version -m "$DIST_DIR/pteron" | awk -v os="GOOS=$HOST_OS" -v arch="GOARCH=$HOST_ARCH" '
        $1 == "build" && $2 == os   { os_ok = 1 }
        $1 == "build" && $2 == arch { arch_ok = 1 }
        END { exit !(os_ok && arch_ok) }'
}

# Write beside the destination so a rename replaces even a running binary.
write_file() (
    local dst=$1 mode=$2 dir tmp
    shift 2
    dir=$(dirname -- "$dst")
    mkdir -p -- "$dir"
    [[ ! -d $dst ]] || die "destination is a directory: $dst"
    tmp=$(mktemp "$dir/.pteron.XXXXXXXX")
    trap 'rm -f -- "$tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    "$@" > "$tmp"
    chmod "$mode" "$tmp"
    mv -f -- "$tmp" "$dst"
    printf 'Installed %s\n' "$dst"
)

write_unit() {
    local line exe=$BIN_DIR/pteron
    if [[ $BIN_DIR -ef $HOME/.local/bin ]]; then
        cat "$SCRIPT_DIR/pteron.service"
        return
    fi
    [[ $exe != *[[:cntrl:]]* ]] || die 'systemd binary paths cannot contain control characters'
    exe=${exe//%/%%}  # Literal systemd specifier marker.
    [[ $exe != *'$'* ]] || exe=:$exe  # Disable systemd environment expansion.
    if [[ $exe == *[[:space:]]* || $exe == *\"* || $exe == *\'* || $exe == *\\* ]]; then
        exe=${exe//\\/\\\\}
        exe=${exe//\"/\\\"}
        exe="\"$exe\""
    fi
    while IFS= read -r line || [[ -n $line ]]; do
        case $line in
            ExecStart=*) printf 'ExecStart=%s serve\n' "$exe" ;;
            *) printf '%s\n' "$line" ;;
        esac
    done < "$SCRIPT_DIR/pteron.service"
}

install_local() {
    local binary=$SCRIPT_DIR/pteron
    if "$source_checkout"; then
        binary=$DIST_DIR/pteron
        if [[ ! -f $binary ]] || ! matches_host; then
            TARGET_OS=$HOST_OS TARGET_ARCH=$HOST_ARCH build
        fi
    fi
    mkdir -p -- "$BIN_DIR"
    BIN_DIR=$(CDPATH= cd -- "$BIN_DIR" && pwd -P)
    write_file "$BIN_DIR/pteron" 0755 cat "$binary"
    if "$skill"; then
        write_file "$SKILL_DIR/pteron/SKILL.md" 0644 cat "$SCRIPT_DIR/skills/pteron/SKILL.md"
    fi
    if "$service"; then
        write_file "$SERVICE_DIR/pteron.service" 0644 write_unit
        if "$restart"; then
            if command -v systemctl >/dev/null && systemctl --user show-environment >/dev/null 2>&1; then
                systemctl --user daemon-reload
                systemctl --user enable pteron.service
                systemctl --user restart pteron.service
            else
                printf 'No active systemd user session. Run pteron serve manually, or enable pteron.service when logged in.\n'
            fi
        fi
    fi
}

if "$source_checkout"; then
    command -v go >/dev/null || die 'Go is required in a source checkout; use a release bundle to install without Go'
    TARGET_OS=${os_flag:-${GOOS:-$(go env GOOS)}}
    TARGET_ARCH=${arch_flag:-${GOARCH:-$(go env GOARCH)}}
fi
if [[ $command_name == all || $command_name == install ]]; then
    if "$source_checkout"; then
        HOST_OS=$(go env GOHOSTOS)
        HOST_ARCH=$(go env GOHOSTARCH)
    elif [[ -n $os_flag || -n $arch_flag ]]; then
        bundle_host
    fi
    if [[ -n $os_flag && $os_flag != "$HOST_OS" || -n $arch_flag && $arch_flag != "$HOST_ARCH" ]]; then
        die "install requires the host target ($HOST_OS/$HOST_ARCH); use package for another target"
    fi
fi

case $command_name in
    all) build; package; install_local ;;
    build) build ;;
    package) build; package ;;
    install) install_local ;;
esac
