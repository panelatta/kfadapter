#!/usr/bin/env sh
# Move a deployment made before the kfadapter -> chinadapter rename onto the
# chinadapter Compose project. The legacy kfadapter_db_data volume is only ever
# read: the stopped legacy state is archived, restored into a fresh
# Compose-managed chinadapter_db_data volume, and validated by the new image.
# Any failure stops the new service and restarts the legacy container.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
PROJECT_NAME=chinadapter
SERVICE=chinadapter
STATE_VOLUME=chinadapter_db_data
LEGACY_VOLUME=kfadapter_db_data
LEGACY_SERVICE_IDENTITY="kfadapter/kfadapter volume:$LEGACY_VOLUME"
BACKUP_DIR=${BACKUP_DIR:-"$PROJECT_ROOT/backups"}
STATE_UID=${CHINADAPTER_HOST_UID:-65532}
STATE_GID=${CHINADAPTER_HOST_GID:-65532}

# compose.yaml names the production project; an inherited override would
# create and start a differently named project and state volume.
unset COMPOSE_PROJECT_NAME

fail() {
    printf '%s\n' "migrate-legacy-state: $*" >&2
    exit 1
}

compose() {
    docker compose --env-file /dev/null -f "$PROJECT_ROOT/compose.yaml" "$@"
}

volume_listed() {
    printf '%s\n' "$available_volumes" | grep -Fx -- "$1" >/dev/null
}

volume_project() {
    docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$1"
}

wait_for_healthy() {
    health_container=$1
    health_failure="did not become healthy within 60 seconds"
    attempt=0
    while [ "$attempt" -lt 12 ]; do
        status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$health_container" 2>/dev/null || true)
        case "$status" in
            healthy)
                return 0
                ;;
            unhealthy|exited|dead|missing|'')
                health_failure="did not become healthy (status: ${status:-missing})"
                return 1
                ;;
        esac
        attempt=$((attempt + 1))
        sleep 5 || {
            health_failure="could not be waited on for health"
            return 1
        }
    done
    return 1
}

verify_state_mount() {
    inspected_container=$1
    state_mount_failure="could not inspect the chinadapter state mount"
    inspected_state_type=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/chinadapter/data"}}{{.Type}}{{end}}{{end}}' "$inspected_container") || return 1
    inspected_state_name=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/chinadapter/data"}}{{.Name}}{{end}}{{end}}' "$inspected_container") || return 1
    inspected_state_source=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/chinadapter/data"}}{{.Source}}{{end}}{{end}}' "$inspected_container") || return 1
    if [ "$inspected_state_type" != volume ] || [ "$inspected_state_name" != "$STATE_VOLUME" ]; then
        state_mount_failure="the chinadapter service does not use the Compose-managed $STATE_VOLUME volume"
        return 1
    fi
    # The service may already be migrating its schema, so a transient journal
    # beside state.db is expected; only the directory identity is compared.
    inspected_state_identity=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
        --state-dir "$inspected_state_source" \
        --base "$PROJECT_ROOT" \
        --uid "$STATE_UID" \
        --gid "$STATE_GID" \
        --allow-damaged-payload 2>/dev/null) || {
        state_mount_failure="the chinadapter state path failed identity validation"
        return 1
    }
    if [ "$inspected_state_identity" != "$state_identity" ]; then
        state_mount_failure="the chinadapter service does not use the restored state directory"
        return 1
    fi
    return 0
}

legacy_container=
legacy_was_running=0
legacy_stop_attempted=0
archive=
archive_ready=0
new_volume_written=0
new_service_started=0
migration_in_progress=0
rollback_in_progress=0
rollback_failure=

rollback() {
    if [ "$new_service_started" -eq 1 ] && ! compose stop "$SERVICE"; then
        rollback_failure="could not stop the new chinadapter service, so the legacy container was left stopped to avoid a listener conflict"
        return 1
    fi
    if [ "$legacy_was_running" -eq 1 ] && [ "$legacy_stop_attempted" -eq 1 ]; then
        if ! docker start "$legacy_container" >/dev/null; then
            rollback_failure="could not restart the legacy kfadapter container $legacy_container"
            return 1
        fi
        if ! wait_for_healthy "$legacy_container"; then
            rollback_failure="the restarted legacy kfadapter container $legacy_container $health_failure"
            return 1
        fi
    fi
    return 0
}

rollback_outcome() {
    if [ "$legacy_was_running" -eq 1 ]; then
        outcome="the legacy kfadapter container was restarted and is healthy"
    elif [ -n "$legacy_container" ]; then
        outcome="the legacy kfadapter container was already stopped and was left stopped"
    else
        outcome="no legacy kfadapter container existed"
    fi
    printf '%s\n' "$outcome"
}

retained_summary() {
    summary="the legacy $LEGACY_VOLUME volume was not modified"
    if [ "$archive_ready" -eq 1 ]; then
        summary="$summary; the pre-migration archive is $archive"
    fi
    if [ "$new_volume_written" -eq 1 ]; then
        summary="$summary; $STATE_VOLUME may hold partially migrated state, so inspect it and remove it with 'docker volume rm $STATE_VOLUME' before retrying"
    fi
    printf '%s\n' "$summary"
}

rollback_or_fail() {
    rollback_in_progress=1
    trap - EXIT HUP INT TERM
    original_failure=$1
    if rollback; then
        printf '%s\n' "migrate-legacy-state: $original_failure; $(rollback_outcome); $(retained_summary)" >&2
    else
        printf '%s\n' "migrate-legacy-state: original failure: $original_failure; rollback failure: $rollback_failure; $(retained_summary)" >&2
    fi
    exit 1
}

handle_migration_exit() {
    exit_status=$1
    trap - EXIT HUP INT TERM
    if [ "$exit_status" -eq 0 ] || [ "$migration_in_progress" -ne 1 ] || [ "$rollback_in_progress" -eq 1 ]; then
        exit "$exit_status"
    fi
    rollback_in_progress=1
    if rollback; then
        printf '%s\n' "migrate-legacy-state: interrupted; $(rollback_outcome); $(retained_summary)" >&2
    else
        printf '%s\n' "migrate-legacy-state: interrupted; rollback failure: $rollback_failure; $(retained_summary)" >&2
    fi
    exit "$exit_status"
}

trap 'handle_migration_exit 129' HUP
trap 'handle_migration_exit 130' INT
trap 'handle_migration_exit 143' TERM
trap 'handle_migration_exit $?' EXIT

[ "$#" -eq 0 ] || {
    printf '%s\n' "usage: set CHINADAPTER_IMAGE_DIGEST=sha256:<digest>; optionally set CHINADAPTER_IMAGE_REPOSITORY=repository (default: ghcr.io/panelatta/chinadapter); scripts/migrate-legacy-state.sh" >&2
    exit 2
}
legacy_names=$(env | sed -n 's/^\(KFADAPTER_[A-Za-z0-9_]*\)=.*/\1/p' | sort -u | tr '\n' ' ')
legacy_names=${legacy_names% }
[ -z "$legacy_names" ] ||
    fail "kfadapter was renamed to chinadapter and these environment variables are no longer read: $legacy_names; set the matching CHINADAPTER_* variables instead (for example CHINADAPTER_IMAGE_DIGEST) and unset the KFADAPTER_* ones"
[ -n "${CHINADAPTER_IMAGE_DIGEST:-}" ] || fail "CHINADAPTER_IMAGE_DIGEST is required"
if ! printf '%s\n' "$CHINADAPTER_IMAGE_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
    fail "CHINADAPTER_IMAGE_DIGEST must be sha256:<64-lowercase-hex>"
fi
if [ -n "${CHINADAPTER_IMAGE_REPOSITORY:-}" ] && ! printf '%s\n' "$CHINADAPTER_IMAGE_REPOSITORY" | grep -Eq '^([a-z0-9][a-z0-9._-]*|[a-z0-9][a-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)+)$'; then
    fail "CHINADAPTER_IMAGE_REPOSITORY must be an untagged registry repository"
fi
[ "$(id -u)" -eq 0 ] || fail "run as root to read the legacy state and preserve numeric state ownership"
[ "$(uname -s)" = Linux ] || fail "native Linux is required; Docker Desktop is unsupported"
command -v python3 >/dev/null 2>&1 || fail "python3 is required for no-follow state validation"
sh "$SCRIPT_DIR/docker-local-context.sh" || exit 1

# The new volume must not hold state that a restore would replace.
available_volumes=$(docker volume ls --format '{{.Name}}') || fail "could not list Docker volumes"
volume_listed "$LEGACY_VOLUME" || fail "legacy Docker volume $LEGACY_VOLUME does not exist; there is no kfadapter state to migrate"
if volume_listed "$STATE_VOLUME"; then
    new_volume_users=$(docker ps -a --filter "volume=$STATE_VOLUME" -q) ||
        fail "could not determine whether $STATE_VOLUME is in use"
    [ -z "$new_volume_users" ] || fail "$STATE_VOLUME already exists and a container uses it; it must be absent, or empty and unused, before migrating"
    existing_project=$(volume_project "$STATE_VOLUME") || fail "could not inspect the existing $STATE_VOLUME volume"
    [ "$existing_project" = "$PROJECT_NAME" ] || fail "$STATE_VOLUME already exists but was not created by the $PROJECT_NAME Compose project; remove it before migrating"
    existing_state_dir=$(sh "$SCRIPT_DIR/state-volume-path.sh") || fail "could not resolve the existing $STATE_VOLUME volume"
    python3 - "$existing_state_dir" <<'PY' || fail "$STATE_VOLUME already exists and is not empty; it must be absent, or empty and unused, before migrating"
import os
import sys

flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
try:
    descriptor = os.open(sys.argv[1], flags)
except OSError as error:
    raise SystemExit(f"migrate-legacy-state: could not open the existing state directory safely: {error}")
try:
    entries = os.listdir(descriptor)
finally:
    os.close(descriptor)
raise SystemExit(1 if entries else 0)
PY
fi

# The legacy state is checked for safe ownership now and for a complete
# state.db once the legacy service has stopped writing it.
legacy_state_dir=$(sh "$SCRIPT_DIR/state-volume-path.sh" --volume "$LEGACY_VOLUME") ||
    fail "could not resolve the legacy $LEGACY_VOLUME volume"
legacy_identity=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
    --state-dir "$legacy_state_dir" \
    --base "$PROJECT_ROOT" \
    --uid "$STATE_UID" \
    --gid "$STATE_GID" \
    --allow-damaged-payload) || fail "legacy state in $LEGACY_VOLUME failed ownership and path validation"

# Identify the legacy service by its Compose labels and state mount, never by
# container name, so that no unrelated container is stopped or restarted.
legacy_containers=$(docker ps -a --filter "volume=$LEGACY_VOLUME" -q) ||
    fail "could not determine which containers use $LEGACY_VOLUME"
if [ -n "$legacy_containers" ]; then
    container_count=$(printf '%s\n' "$legacy_containers" | wc -l | tr -d ' ')
    [ "$container_count" = 1 ] || fail "more than one container uses $LEGACY_VOLUME; remove every container except the kfadapter Compose service before migrating"
    legacy_identity_labels=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}} {{range .Mounts}}{{if eq .Destination "/kfadapter/data"}}{{.Type}}:{{.Name}}{{end}}{{end}}' "$legacy_containers") ||
        fail "could not inspect the container that uses $LEGACY_VOLUME"
    [ "$legacy_identity_labels" = "$LEGACY_SERVICE_IDENTITY" ] ||
        fail "the container that uses $LEGACY_VOLUME is not the kfadapter Compose service with its state at /kfadapter/data"
    legacy_container=$legacy_containers
    legacy_running=$(docker inspect --format '{{.State.Running}}' "$legacy_container") ||
        fail "could not inspect whether the legacy kfadapter container is running"
    if [ "$legacy_running" = true ]; then
        legacy_was_running=1
    fi
fi

[ -n "$BACKUP_DIR" ] || fail "BACKUP_DIR must not be empty"
case "$BACKUP_DIR" in
    /*) backup_archive_dir=$BACKUP_DIR ;;
    *) backup_archive_dir=$PROJECT_ROOT/$BACKUP_DIR ;;
esac
archive_nonce=$(python3 -c 'import secrets; print(secrets.token_hex(16))') ||
    fail "could not generate a unique pre-migration archive name"
archive="$backup_archive_dir/pre-chinadapter-$archive_nonce.tar.gz"

# Fetch the new image while the legacy service is still serving.
compose config --quiet || fail "Compose rejected the chinadapter image inputs"
compose pull "$SERVICE" || fail "could not pull the requested chinadapter image"

migration_in_progress=1
if [ "$legacy_was_running" -eq 1 ]; then
    legacy_stop_attempted=1
    docker stop "$legacy_container" >/dev/null || rollback_or_fail "could not stop the legacy kfadapter container"
fi
legacy_users=$(docker ps --filter "volume=$LEGACY_VOLUME" -q) ||
    rollback_or_fail "could not confirm that $LEGACY_VOLUME is no longer in use"
[ -z "$legacy_users" ] || rollback_or_fail "$LEGACY_VOLUME is still in use after stopping the legacy container"
legacy_identity=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
    --state-dir "$legacy_state_dir" \
    --base "$PROJECT_ROOT" \
    --uid "$STATE_UID" \
    --gid "$STATE_GID" \
    --expect-identity "$legacy_identity") ||
    rollback_or_fail "stopped legacy state is not exactly one bounded state.db with the state owner"
legacy_state_dir=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
    --state-dir "$legacy_state_dir" \
    --base "$PROJECT_ROOT" \
    --uid "$STATE_UID" \
    --gid "$STATE_GID" \
    --expect-identity "$legacy_identity" \
    --print-canonical-path) ||
    rollback_or_fail "could not canonicalize the verified legacy state directory"

# The archive is the offline copy that is restored; it also remains as the
# protected pre-migration backup.
python3 "$SCRIPT_DIR/backup-state-write.py" \
    --project-root "$PROJECT_ROOT" \
    --state-dir "$legacy_state_dir" \
    --state-identity "$legacy_identity" \
    --backup-dir "$BACKUP_DIR" \
    --archive "$archive" >/dev/null ||
    rollback_or_fail "could not create the pre-migration state archive"
archive_ready=1

# A one-off container makes Compose create the labelled project volume; Docker
# copies the image's empty 0700 65532:65532 /chinadapter/data into it.
new_volume_written=1
compose run --rm --no-deps --entrypoint true "$SERVICE" ||
    rollback_or_fail "could not create the Compose-managed $STATE_VOLUME volume"
new_volume_project=$(volume_project "$STATE_VOLUME") ||
    rollback_or_fail "could not inspect the new $STATE_VOLUME volume"
[ "$new_volume_project" = "$PROJECT_NAME" ] ||
    rollback_or_fail "$STATE_VOLUME is not labelled as the $PROJECT_NAME Compose project volume"

# restore-state.sh validates the archive with the new image, whose
# validate-state migrates older schemas on a temporary copy.
"$SCRIPT_DIR/restore-state.sh" "$archive" ||
    rollback_or_fail "could not restore the legacy state into $STATE_VOLUME"
state_dir=$(sh "$SCRIPT_DIR/state-volume-path.sh") ||
    rollback_or_fail "could not resolve the restored $STATE_VOLUME volume"
state_identity=$(python3 "$SCRIPT_DIR/verify-state-path.py" \
    --state-dir "$state_dir" \
    --base "$PROJECT_ROOT" \
    --uid "$STATE_UID" \
    --gid "$STATE_GID") ||
    rollback_or_fail "restored state failed ownership and layout validation"

new_service_started=1
compose up -d --pull never "$SERVICE" || rollback_or_fail "could not start the chinadapter service"
new_container=$(compose ps -q "$SERVICE" 2>/dev/null || true)
[ -n "$new_container" ] || rollback_or_fail "the chinadapter container was not created"
verify_state_mount "$new_container" || rollback_or_fail "$state_mount_failure"
wait_for_healthy "$new_container" || rollback_or_fail "the chinadapter service $health_failure"
migration_in_progress=0

printf '%s\n' "migrate-legacy-state: chinadapter is healthy on the $STATE_VOLUME volume"
printf '%s\n' "migrate-legacy-state: the pre-migration archive is $archive"
printf '%s\n' "migrate-legacy-state: the legacy $LEGACY_VOLUME volume was not modified and is kept for rollback."
printf '%s\n' "migrate-legacy-state: once the migration is confirmed, remove the legacy deployment with:"
if [ -n "$legacy_container" ]; then
    printf '  %s\n' "docker rm $legacy_container"
fi
printf '%s\n' "migrate-legacy-state: and later, when you no longer need to roll back:"
printf '  %s\n' "docker volume rm $LEGACY_VOLUME"
