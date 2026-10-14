#!/usr/bin/env sh
# Exercise the kfadapter -> chinadapter state migration and its rollback with a
# fake Docker Engine. The real archive, restore, and validation helpers run.
set -eu
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE_DIGEST=sha256:3333333333333333333333333333333333333333333333333333333333333333

fail() {
    printf '%s\n' "migrate-legacy-state-test: $*" >&2
    exit 1
}

tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/chinadapter-migrate-legacy.XXXXXXXX")
work=$(CDPATH= cd -- "$work" && pwd -P)
cleanup() {
    rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM

actual_uid=$(id -u)
actual_gid=$(id -g)
legacy_dir="$work/volumes/kfadapter_db_data/_data"
new_dir="$work/volumes/chinadapter_db_data/_data"
mkdir -p "$work/bin" "$work/scripts" "$work/fake"
: >"$work/compose.yaml"
cp "$PROJECT_ROOT/scripts/migrate-legacy-state.sh" \
    "$PROJECT_ROOT/scripts/docker-local-context.sh" \
    "$PROJECT_ROOT/scripts/state-volume-path.sh" \
    "$PROJECT_ROOT/scripts/verify-state-path.py" \
    "$PROJECT_ROOT/scripts/backup-state-write.py" \
    "$PROJECT_ROOT/scripts/restore-state.sh" \
    "$PROJECT_ROOT/scripts/restore-state-archive.py" \
    "$PROJECT_ROOT/scripts/restore-state-commit.py" \
    "$work/scripts/"
cp "$PROJECT_ROOT/scripts/preflight.sh" "$work/scripts/preflight-real.sh"
cat >"$work/scripts/preflight.sh" <<'SH'
#!/usr/bin/env sh
printf '%s\n' "preflight $*" >>"$FAKE_LOG"
exec "$(dirname -- "$0")/preflight-real.sh" "$@"
SH

# A pre-rename deployment's rollback-journal SQLite state.
python3 - "$work/legacy-state.db" <<'PY'
from pathlib import Path
import sqlite3
import sys

database = Path(sys.argv[1])
connection = sqlite3.connect(database)
connection.execute("PRAGMA journal_mode=DELETE")
connection.execute("PRAGMA user_version=10")
connection.execute("CREATE TABLE deployment_verification (marker TEXT NOT NULL)")
connection.execute("INSERT INTO deployment_verification(marker) VALUES ('legacy-state')")
connection.commit()
connection.execute("VACUUM")
connection.close()
database.chmod(0o600)
PY

cat >"$work/bin/uname" <<'SH'
#!/usr/bin/env sh
printf '%s\n' Linux
SH
cat >"$work/bin/id" <<'SH'
#!/usr/bin/env sh
set -eu
case "${1:-}" in
    -u) printf '%s\n' "${FAKE_ID_UID:-0}" ;;
    *) exit 2 ;;
esac
SH
cat >"$work/bin/docker" <<'SH'
#!/usr/bin/env sh
set -eu
root=${FAKE_ROOT:?}
printf '%s\n' "$*" >>"$root/calls"
log() {
    printf '%s\n' "$*" >>"$FAKE_LOG"
}
volume_listed() {
    grep -Fx -- "$1" "$root/fake/volumes" >/dev/null 2>&1
}
legacy=$(cat "$root/fake/legacy")
new=$(cat "$root/fake/new")
image="ghcr.io/panelatta/chinadapter@${CHINADAPTER_IMAGE_DIGEST:?}"
legacy_containers=${FAKE_LEGACY_CONTAINERS:-one}

case "${1:-}" in
    version)
        [ "${2:-}" = --format ] && [ "${3:-}" = '{{.Server.Os}}' ] || exit 2
        printf '%s\n' linux
        ;;
    info)
        [ "${2:-}" = --format ] && [ "${3:-}" = '{{.OperatingSystem}}' ] || exit 2
        printf '%s\n' 'Debian GNU/Linux'
        ;;
    context)
        [ "${2:-}" = inspect ] && [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Endpoints.docker.Host}}' ] || exit 2
        printf '%s\n' unix:///var/run/docker.sock
        ;;
    volume)
        case "${2:-}" in
            ls)
                [ "$#" -eq 4 ] && [ "$3" = --format ] && [ "$4" = '{{.Name}}' ] || exit 2
                cat "$root/fake/volumes"
                ;;
            inspect)
                [ "$#" -eq 5 ] && [ "$3" = --format ] || exit 2
                name=$5
                case "$name" in
                    chinadapter_db_data|kfadapter_db_data) ;;
                    *) exit 2 ;;
                esac
                volume_listed "$name" || exit 1
                case "$4" in
                    '{{.Name}}') printf '%s\n' "$name" ;;
                    '{{.Driver}}') printf '%s\n' local ;;
                    '{{.Mountpoint}}') printf '%s\n' "$root/volumes/$name/_data" ;;
                    '{{json .Options}}') printf '%s\n' null ;;
                    '{{index .Labels "com.docker.compose.project"}}')
                        if [ "$name" = chinadapter_db_data ]; then
                            printf '%s\n' "${FAKE_NEW_VOLUME_PROJECT:-chinadapter}"
                        else
                            printf '%s\n' kfadapter
                        fi
                        ;;
                    *) exit 2 ;;
                esac
                ;;
            *) exit 2 ;;
        esac
        ;;
    ps)
        shift
        all=0
        if [ "${1:-}" = -a ]; then
            all=1
            shift
        fi
        [ "$#" -eq 3 ] && [ "$1" = --filter ] && [ "$3" = -q ] || exit 2
        case "$2" in
            volume=kfadapter_db_data)
                if [ "$legacy_containers" != none ] && { [ "$all" -eq 1 ] || [ "$legacy" = running ]; }; then
                    printf '%s\n' legacy-container
                fi
                if [ "$legacy_containers" = two ] && [ "$all" -eq 1 ]; then
                    printf '%s\n' other-container
                fi
                ;;
            volume=chinadapter_db_data)
                if [ "${FAKE_NEW_VOLUME_USERS:-}" = 1 ] && [ "$all" -eq 1 ]; then
                    printf '%s\n' foreign-container
                fi
                if [ "$new" = running ] || { [ "$new" = stopped ] && [ "$all" -eq 1 ]; }; then
                    printf '%s\n' new-container
                fi
                ;;
            *) exit 2 ;;
        esac
        ;;
    inspect)
        [ "$#" -eq 4 ] && [ "$2" = --format ] || exit 2
        format=$3
        container=$4
        case "$container:$format" in
            legacy-container:*com.docker.compose.project*com.docker.compose.service*'Destination "/kfadapter/data"'*)
                printf '%s\n' "${FAKE_LEGACY_IDENTITY:-kfadapter/kfadapter volume:kfadapter_db_data}"
                ;;
            legacy-container:'{{.State.Running}}')
                if [ "$legacy" = running ]; then printf '%s\n' true; else printf '%s\n' false; fi
                ;;
            legacy-container:*State.Health*)
                if [ "$legacy" = running ]; then printf '%s\n' healthy; else printf '%s\n' exited; fi
                ;;
            new-container:*State.Health*)
                if [ "$new" != running ]; then
                    printf '%s\n' exited
                elif [ "${FAIL_PHASE:-}" = health ]; then
                    printf '%s\n' unhealthy
                else
                    printf '%s\n' healthy
                fi
                ;;
            new-container:*'Destination "/chinadapter/data"'*'{{.Type}}'*) printf '%s\n' volume ;;
            new-container:*'Destination "/chinadapter/data"'*'{{.Name}}'*) printf '%s\n' "${FAKE_NEW_MOUNT_NAME:-chinadapter_db_data}" ;;
            new-container:*'Destination "/chinadapter/data"'*'{{.Source}}'*) printf '%s\n' "$root/volumes/chinadapter_db_data/_data" ;;
            *) exit 2 ;;
        esac
        ;;
    stop)
        [ "$#" -eq 2 ] && [ "$2" = legacy-container ] || exit 2
        log "docker stop legacy-container"
        [ "${FAIL_PHASE:-}" != stop ] || exit 1
        printf '%s\n' stopped >"$root/fake/legacy"
        printf '%s\n' legacy-container
        ;;
    start)
        [ "$#" -eq 2 ] && [ "$2" = legacy-container ] || exit 2
        log "docker start legacy-container"
        [ "${FAIL_ROLLBACK:-}" != start ] || exit 1
        # Both services listen on the same host-network ports.
        [ "$new" != running ] || exit 1
        printf '%s\n' running >"$root/fake/legacy"
        printf '%s\n' legacy-container
        ;;
    image)
        [ "$#" -eq 3 ] && [ "$2" = inspect ] && [ "$3" = "$image" ] || exit 2
        log "image inspect"
        ;;
    run)
        tmpfs=
        mount=
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --tmpfs) tmpfs=$2; shift 2 ;;
                -v) mount=$2; shift 2 ;;
                *) shift ;;
            esac
        done
        # validate-state migrates older schemas on a temporary copy.
        [ "$tmpfs" = /tmp:rw,noexec,nosuid,nodev,size=40m ] || exit 2
        case "$mount" in /*:/restore:ro) ;; *) exit 2 ;; esac
        log "validate-state"
        [ "${FAIL_PHASE:-}" != validate ] || exit 1
        cmp -s "${mount%:/restore:ro}/state.db" "${FAKE_LEGACY_DB:?}" || exit 1
        ;;
    compose)
        shift
        [ "${1:-}" != version ] || exit 0
        [ "$#" -ge 5 ] && [ "$1" = --env-file ] && [ "$2" = /dev/null ] && [ "$3" = -f ] && [ "$4" = "$root/compose.yaml" ] || exit 2
        shift 4
        case "$*" in
            'config --quiet')
                log "compose config"
                ;;
            'config --images chinadapter')
                printf '%s\n' "$image"
                ;;
            'pull chinadapter')
                log "compose pull"
                [ "${FAIL_PHASE:-}" != pull ] || exit 1
                ;;
            'run --rm --no-deps --entrypoint true chinadapter')
                log "compose run"
                # Compose creates the labelled volume; Docker copies up the
                # image's empty 0700 state directory.
                mkdir -p "$root/volumes/chinadapter_db_data/_data"
                chmod 0700 "$root/volumes/chinadapter_db_data/_data"
                volume_listed chinadapter_db_data || printf '%s\n' chinadapter_db_data >>"$root/fake/volumes"
                if [ "${FAKE_INTERRUPT_ON:-}" = run ]; then
                    log "interrupt TERM during compose run"
                    kill -TERM "$PPID"
                fi
                [ "${FAIL_PHASE:-}" != run ] || exit 1
                ;;
            'up -d --pull never chinadapter')
                log "compose up"
                [ "$legacy" != running ] || exit 1
                [ "${FAIL_PHASE:-}" != up ] || exit 1
                # The new image migrates the schema in place on first start.
                journal="$root/volumes/chinadapter_db_data/_data/state.db-journal"
                printf '%s\n' 'schema migration in progress' >"$journal"
                chmod 0600 "$journal"
                printf '%s\n' running >"$root/fake/new"
                ;;
            'ps -q chinadapter')
                [ "$new" != running ] || printf '%s\n' new-container
                ;;
            'stop chinadapter')
                log "compose stop"
                [ "$new" = absent ] || printf '%s\n' stopped >"$root/fake/new"
                ;;
            *) exit 2 ;;
        esac
        ;;
    *) exit 2 ;;
esac
SH
chmod 0755 "$work/bin/uname" "$work/bin/id" "$work/bin/docker" "$work/scripts/"*

clear_knobs() {
    unset FAKE_LEGACY_CONTAINERS FAKE_LEGACY_IDENTITY FAKE_NEW_VOLUME_USERS FAKE_NEW_VOLUME_PROJECT \
        FAKE_NEW_MOUNT_NAME FAIL_PHASE FAIL_ROLLBACK FAKE_INTERRUPT_ON FAKE_ID_UID MIGRATE_DIGEST
}

reset_fixture() {
    clear_knobs
    rm -rf -- "$work/volumes" "$work/backups"
    mkdir -p "$legacy_dir"
    chmod 0700 "$legacy_dir"
    cp "$work/legacy-state.db" "$legacy_dir/state.db"
    chmod 0600 "$legacy_dir/state.db"
    printf '%s\n' kfadapter_db_data >"$work/fake/volumes"
    printf '%s\n' running >"$work/fake/legacy"
    printf '%s\n' absent >"$work/fake/new"
    : >"$work/log"
    : >"$work/calls"
    legacy_inode=$(python3 -c 'import os, sys; print(os.lstat(sys.argv[1]).st_ino)' "$legacy_dir")
}

run_migrate() {
    (
        unset CHINADAPTER_IMAGE_REPOSITORY COMPOSE_PROJECT_NAME
        PATH="$work/bin:$PATH" \
            FAKE_ROOT="$work" \
            FAKE_LOG="$work/log" \
            FAKE_LEGACY_DB="$work/legacy-state.db" \
            FAKE_LEGACY_CONTAINERS="${FAKE_LEGACY_CONTAINERS:-one}" \
            FAKE_LEGACY_IDENTITY="${FAKE_LEGACY_IDENTITY:-}" \
            FAKE_NEW_VOLUME_USERS="${FAKE_NEW_VOLUME_USERS:-}" \
            FAKE_NEW_VOLUME_PROJECT="${FAKE_NEW_VOLUME_PROJECT:-}" \
            FAKE_NEW_MOUNT_NAME="${FAKE_NEW_MOUNT_NAME:-}" \
            FAIL_PHASE="${FAIL_PHASE:-}" \
            FAIL_ROLLBACK="${FAIL_ROLLBACK:-}" \
            FAKE_INTERRUPT_ON="${FAKE_INTERRUPT_ON:-}" \
            FAKE_ID_UID="${FAKE_ID_UID:-0}" \
            CHINADAPTER_HOST_UID="$actual_uid" \
            CHINADAPTER_HOST_GID="$actual_gid" \
            CHINADAPTER_IMAGE_DIGEST="${MIGRATE_DIGEST-$IMAGE_DIGEST}" \
            "$work/scripts/migrate-legacy-state.sh"
    )
}

assert_legacy_untouched() {
    cmp -s "$work/legacy-state.db" "$legacy_dir/state.db" || fail "$1: legacy state bytes changed"
    [ "$(python3 -c 'import os, sys; print(os.lstat(sys.argv[1]).st_ino)' "$legacy_dir")" = "$legacy_inode" ] ||
        fail "$1: legacy state directory was replaced"
}

assert_no_destructive_calls() {
    if grep -Eq '(^| )(down|rm|prune)( |$)' "$work/calls"; then
        fail "$1: Docker containers or volumes were removed"
    fi
}

assert_legacy_running() {
    [ "$(cat "$work/fake/legacy")" = running ] || fail "$1: legacy container is not running"
}

assert_untouched_rejection() {
    name=$1
    diagnostic=$2
    if run_migrate >"$work/output" 2>&1; then
        fail "$name: migration was accepted"
    fi
    grep -Fq -- "$diagnostic" "$work/output" || fail "$name: rejection was not explained"
    if grep -Fq 'docker stop' "$work/log"; then
        fail "$name: rejection stopped the legacy container"
    fi
    if grep -Fq 'compose ' "$work/log"; then
        fail "$name: rejection reached Compose"
    fi
    [ ! -e "$work/backups" ] || fail "$name: rejection created an archive"
    assert_legacy_running "$name"
    assert_legacy_untouched "$name"
    assert_no_destructive_calls "$name"
}

assert_success() {
    name=$1
    grep -Fq 'migrate-legacy-state: chinadapter is healthy on the chinadapter_db_data volume' "$work/output" || fail "$name: success was not reported"
    cmp -s "$work/legacy-state.db" "$new_dir/state.db" || fail "$name: migrated state bytes differ from the legacy state"
    python3 - "$new_dir" "$actual_uid" "$actual_gid" <<'PY' || fail "$name: migrated state ownership or mode is wrong"
from pathlib import Path
import stat
import sys

state = Path(sys.argv[1])
uid, gid = int(sys.argv[2]), int(sys.argv[3])
directory = state.lstat()
assert stat.S_IMODE(directory.st_mode) == 0o700 and (directory.st_uid, directory.st_gid) == (uid, gid)
database = (state / "state.db").lstat()
assert stat.S_ISREG(database.st_mode) and stat.S_IMODE(database.st_mode) == 0o600
assert (database.st_uid, database.st_gid) == (uid, gid)
PY
    set -- "$work"/backups/pre-chinadapter-*.tar.gz
    [ "$#" -eq 1 ] && [ -f "$1" ] || fail "$name: one pre-migration archive was not created"
    grep -Fq "the pre-migration archive is $1" "$work/output" || fail "$name: archive path was not reported"
    python3 - "$1" "$work/legacy-state.db" <<'PY' || fail "$name: pre-migration archive does not hold the exact legacy state"
from pathlib import Path
import stat
import sys
from tarfile import open as taropen

expected = Path(sys.argv[2]).read_bytes()
with taropen(sys.argv[1], "r:gz") as bundle:
    members = bundle.getmembers()
    assert len(members) == 1
    member = members[0]
    assert member.name == "state.db" and member.isreg() and stat.S_IMODE(member.mode) == 0o600
    payload = bundle.extractfile(member)
    assert payload is not None and payload.read() == expected
PY
    grep -Fq 'preflight --restore-existing --state-dir' "$work/log" || fail "$name: restore did not run recovery preflight"
    grep -Fq 'docker volume rm kfadapter_db_data' "$work/output" || fail "$name: legacy volume cleanup was not explained"
    [ "$(cat "$work/fake/new")" = running ] || fail "$name: chinadapter is not running"
    if grep -Fq 'docker start' "$work/log"; then
        fail "$name: successful migration restarted the legacy container"
    fi
    assert_legacy_untouched "$name"
    assert_no_destructive_calls "$name"
}

assert_order() {
    python3 - "$work/log" "$@" <<'PY' || fail "migration steps ran out of order: $*"
from pathlib import Path
import sys

lines = Path(sys.argv[1]).read_text().splitlines()
positions = [lines.index(step) for step in sys.argv[2:]]
assert positions == sorted(positions), positions
PY
}

assert_rolled_back() {
    name=$1
    diagnostic=$2
    if run_migrate >"$work/output" 2>&1; then
        fail "$name: failing migration returned success"
    fi
    grep -Fq -- "$diagnostic" "$work/output" || fail "$name: original failure was not reported"
    grep -Fq 'the legacy kfadapter container was restarted and is healthy' "$work/output" || fail "$name: rollback was not reported"
    grep -Fq 'the legacy kfadapter_db_data volume was not modified' "$work/output" || fail "$name: legacy volume safety was not reported"
    assert_legacy_running "$name"
    [ "$(cat "$work/fake/new")" != running ] || fail "$name: rollback left chinadapter running"
    assert_legacy_untouched "$name"
    assert_no_destructive_calls "$name"
}

# A running legacy service is stopped only after the new image is pulled,
# archived offline, restored through the new image's validator, and replaced.
run_successful_migration() {
    run_migrate >"$work/output" 2>&1 || {
        cat "$work/output" >&2
        fail "$1: migration failed"
    }
    assert_success "$1"
}

reset_fixture
run_successful_migration running-legacy
assert_order "compose config" "compose pull" "docker stop legacy-container" "compose run" "image inspect" "validate-state" "compose up"
grep -Fq '  docker rm legacy-container' "$work/output" || fail "legacy container cleanup was not explained"
[ "$(cat "$work/fake/legacy")" = stopped ] || fail "legacy container was not left stopped"

# A legacy service that was already stopped is neither stopped nor restarted.
reset_fixture
printf '%s\n' stopped >"$work/fake/legacy"
run_successful_migration stopped-legacy
if grep -Fq 'docker stop' "$work/log"; then
    fail "migration stopped an already stopped legacy container"
fi
grep -Fq '  docker rm legacy-container' "$work/output" || fail "stopped legacy container cleanup was not explained"

# Only the volume remains (for example after compose down without -v).
reset_fixture
FAKE_LEGACY_CONTAINERS=none
printf '%s\n' absent >"$work/fake/legacy"
run_successful_migration volume-only
if grep -Fq 'docker rm' "$work/output"; then
    fail "migration without a legacy container suggested removing one"
fi

# An existing empty, unused, Compose-labelled new volume is acceptable.
reset_fixture
mkdir -p "$new_dir"
chmod 0700 "$new_dir"
printf '%s\n' chinadapter_db_data >>"$work/fake/volumes"
run_successful_migration empty-new-volume

# Preconditions fail before the legacy service is touched.
reset_fixture
mkdir -p "$new_dir"
chmod 0700 "$new_dir"
cp "$work/legacy-state.db" "$new_dir/state.db"
printf '%s\n' chinadapter_db_data >>"$work/fake/volumes"
assert_untouched_rejection populated-new-volume 'chinadapter_db_data already exists and is not empty'
cmp -s "$work/legacy-state.db" "$new_dir/state.db" || fail "rejected migration changed existing chinadapter state"

reset_fixture
mkdir -p "$new_dir"
chmod 0700 "$new_dir"
printf '%s\n' chinadapter_db_data >>"$work/fake/volumes"
FAKE_NEW_VOLUME_USERS=1
assert_untouched_rejection used-new-volume 'chinadapter_db_data already exists and a container uses it'

reset_fixture
mkdir -p "$new_dir"
chmod 0700 "$new_dir"
printf '%s\n' chinadapter_db_data >>"$work/fake/volumes"
FAKE_NEW_VOLUME_PROJECT=other
assert_untouched_rejection unlabelled-new-volume 'was not created by the chinadapter Compose project'

reset_fixture
FAKE_LEGACY_IDENTITY='other/kfadapter volume:kfadapter_db_data'
assert_untouched_rejection foreign-project 'is not the kfadapter Compose service'

reset_fixture
FAKE_LEGACY_IDENTITY='kfadapter/kfadapter bind:'
assert_untouched_rejection bind-mounted-legacy 'is not the kfadapter Compose service'

reset_fixture
FAKE_LEGACY_CONTAINERS=two
assert_untouched_rejection two-legacy-containers 'more than one container uses kfadapter_db_data'

reset_fixture
: >"$work/fake/volumes"
assert_untouched_rejection no-legacy-volume 'there is no kfadapter state to migrate'

reset_fixture
chmod 0755 "$legacy_dir"
assert_untouched_rejection unsafe-legacy-state 'failed ownership and path validation'
chmod 0700 "$legacy_dir"

for rejected in missing-digest bad-digest non-root legacy-environment; do
    reset_fixture
    case "$rejected" in
        missing-digest) MIGRATE_DIGEST= ; diagnostic='CHINADAPTER_IMAGE_DIGEST is required' ;;
        bad-digest) MIGRATE_DIGEST=sha256:0 ; diagnostic='CHINADAPTER_IMAGE_DIGEST must be sha256:<64-lowercase-hex>' ;;
        non-root) FAKE_ID_UID=501 ; diagnostic='run as root' ;;
        legacy-environment) diagnostic='no longer read: KFADAPTER_IMAGE_DIGEST;' ;;
    esac
    if [ "$rejected" = legacy-environment ]; then
        if (
            export KFADAPTER_IMAGE_DIGEST="$IMAGE_DIGEST"
            run_migrate
        ) >"$work/output" 2>&1; then
            fail "$rejected: migration was accepted"
        fi
    elif run_migrate >"$work/output" 2>&1; then
        fail "$rejected: migration was accepted"
    fi
    grep -Fq -- "$diagnostic" "$work/output" || fail "$rejected: rejection was not explained"
    [ ! -s "$work/calls" ] || fail "$rejected: rejection reached Docker"
    assert_legacy_untouched "$rejected"
done

# The new image is pulled while the legacy service still serves.
reset_fixture
FAIL_PHASE=pull
if run_migrate >"$work/output" 2>&1; then
    fail "pull failure did not fail the migration"
fi
grep -Fq 'could not pull the requested chinadapter image' "$work/output" || fail "pull failure was not reported"
if grep -Fq 'docker stop' "$work/log"; then
    fail "pull failure stopped the legacy container"
fi
[ ! -e "$work/backups" ] || fail "pull failure created an archive"
if grep -Fqx chinadapter_db_data "$work/fake/volumes"; then
    fail "pull failure created chinadapter_db_data"
fi
assert_legacy_running pull-failure
assert_legacy_untouched pull-failure
assert_no_destructive_calls pull-failure

# Failures after the legacy service stopped restart it; the legacy volume is
# never written, so that restart is a complete rollback.
reset_fixture
FAIL_PHASE=stop
assert_rolled_back stop-failure 'could not stop the legacy kfadapter container'

reset_fixture
printf '%s\n' 'interrupted SQLite journal' >"$legacy_dir/state.db-journal"
chmod 0600 "$legacy_dir/state.db-journal"
legacy_inode=$(python3 -c 'import os, sys; print(os.lstat(sys.argv[1]).st_ino)' "$legacy_dir")
assert_rolled_back legacy-journal 'stopped legacy state is not exactly one bounded state.db'
[ -f "$legacy_dir/state.db-journal" ] || fail "legacy journal was removed"
[ ! -e "$work/backups" ] || fail "incomplete legacy state was archived"

reset_fixture
FAIL_PHASE=run
assert_rolled_back volume-creation-failure 'could not create the Compose-managed chinadapter_db_data volume'
grep -Fq "remove it with 'docker volume rm chinadapter_db_data' before retrying" "$work/output" ||
    fail "volume creation failure did not explain the new volume cleanup"

reset_fixture
FAIL_PHASE=validate
assert_rolled_back validation-failure 'could not restore the legacy state into chinadapter_db_data'
if grep -Fq 'compose up' "$work/log"; then
    fail "validation failure started chinadapter"
fi
[ ! -e "$new_dir/state.db" ] || fail "validation failure installed state into chinadapter_db_data"
set -- "$work"/backups/pre-chinadapter-*.tar.gz
[ "$#" -eq 1 ] && [ -f "$1" ] || fail "validation failure did not keep the pre-migration archive"
grep -Fq "the pre-migration archive is $1" "$work/output" || fail "validation failure did not report the archive"

reset_fixture
FAIL_PHASE=up
assert_rolled_back start-failure 'could not start the chinadapter service'
assert_order "compose up" "compose stop" "docker start legacy-container"

reset_fixture
FAIL_PHASE=health
assert_rolled_back health-failure 'the chinadapter service did not become healthy (status: unhealthy)'
assert_order "compose up" "compose stop" "docker start legacy-container"

reset_fixture
FAKE_NEW_MOUNT_NAME=other_db_data
assert_rolled_back mount-mismatch 'does not use the Compose-managed chinadapter_db_data volume'

reset_fixture
FAKE_INTERRUPT_ON=run
if run_migrate >"$work/output" 2>&1; then
    fail "interrupted migration returned success"
else
    interrupt_status=$?
fi
[ "$interrupt_status" -eq 143 ] || fail "interrupted migration returned status $interrupt_status instead of 143"
grep -Fq 'interrupted; the legacy kfadapter container was restarted and is healthy' "$work/output" || fail "interrupted migration did not report its rollback"
if grep -Fq 'compose up' "$work/log"; then
    fail "interrupted migration started chinadapter"
fi
assert_legacy_running interrupted
assert_legacy_untouched interrupted
assert_no_destructive_calls interrupted

reset_fixture
FAIL_PHASE=health
FAIL_ROLLBACK=start
if run_migrate >"$work/output" 2>&1; then
    fail "rollback failure returned success"
fi
grep -Fq 'original failure: the chinadapter service did not become healthy' "$work/output" || fail "rollback failure hid the original failure"
grep -Fq 'rollback failure: could not restart the legacy kfadapter container legacy-container' "$work/output" || fail "rollback failure was not reported"
[ "$(cat "$work/fake/new")" = stopped ] || fail "rollback failure left chinadapter running"
assert_legacy_untouched rollback-failure
assert_no_destructive_calls rollback-failure

printf '%s\n' "migrate-legacy-state-test: passed"
