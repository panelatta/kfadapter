#!/usr/bin/env sh
# Create an owner-preserving state archive without following output paths. A
# running service is backed up online through `kfadapter backup`, which streams
# a validated point-in-time snapshot; a stopped service is copied offline.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
STATE_VOLUME=kfadapter_db_data
SERVICE_IDENTITY="kfadapter/kfadapter volume:$STATE_VOLUME"
BACKUP_DIR=${BACKUP_DIR:-"$PROJECT_ROOT/backups"}
STATE_UID=${KFADAPTER_HOST_UID:-65532}
STATE_GID=${KFADAPTER_HOST_GID:-65532}

fail() {
    printf '%s\n' "backup-state: $*" >&2
    exit 1
}

usage() {
    printf '%s\n' "usage: scripts/backup-state.sh [--online|--offline] [archive.tar.gz]" >&2
    exit 2
}

mode=auto
case "${1:-}" in
    --online) mode=online; shift ;;
    --offline) mode=offline; shift ;;
    -*) usage ;;
esac
[ "$#" -le 1 ] || usage
state_dir=$(sh "$SCRIPT_DIR/state-volume-path.sh") || fail "could not resolve Docker state volume"
[ -n "$BACKUP_DIR" ] || fail "BACKUP_DIR must not be empty"
ARCHIVE=${1:-}

command -v python3 >/dev/null 2>&1 || fail "python3 is required for no-follow state backup"
running_services=$(docker ps --filter "volume=$STATE_VOLUME" -q) ||
    fail "could not determine whether the Docker state volume is in use"
if [ "$mode" = auto ]; then
    if [ -n "$running_services" ]; then
        mode=online
    else
        mode=offline
    fi
fi

if [ "$mode" = online ]; then
    [ -n "$running_services" ] || fail "no running service mounts the Docker state volume; use --offline for a stopped service"
    container_count=$(printf '%s\n' "$running_services" | wc -l | tr -d ' ')
    [ "$container_count" = 1 ] || fail "more than one container mounts the Docker state volume; stop the others before an online backup"
    # The snapshot runs inside the container, so it must be the Compose service
    # whose ./data is this volume, not merely some container that mounts it.
    identity=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}} {{range .Mounts}}{{if eq .Destination "/kfadapter/data"}}{{.Type}}:{{.Name}}{{end}}{{end}}' "$running_services") ||
        fail "could not inspect the running service"
    [ "$identity" = "$SERVICE_IDENTITY" ] || fail "the container mounting the Docker state volume is not the kfadapter service with its state at /kfadapter/data"
    set -- --online-container "$running_services" --uid "$STATE_UID" --gid "$STATE_GID"
else
    [ -z "$running_services" ] || fail "Docker state volume is in use; stop every container mounting it before creating an offline state backup"
    state_identity=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
        --state-dir "$state_dir" \
        --base "$PROJECT_ROOT" \
        --uid "$STATE_UID" \
        --gid "$STATE_GID")
    state_dir=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
        --state-dir "$state_dir" \
        --base "$PROJECT_ROOT" \
        --uid "$STATE_UID" \
        --gid "$STATE_GID" \
        --expect-identity "$state_identity" \
        --print-canonical-path)
    "$SCRIPT_DIR/preflight.sh" --state-only --state-dir "$state_dir"
    set -- --state-identity "$state_identity"
fi

if [ -n "$ARCHIVE" ]; then
    set -- "$@" --archive "$ARCHIVE"
fi
python3 "$SCRIPT_DIR/backup-state-write.py" \
    --project-root "$PROJECT_ROOT" \
    --state-dir "$state_dir" \
    --backup-dir "$BACKUP_DIR" \
    "$@" >/dev/null
printf '%s\n' "backup-state: created protected $mode state archive"
