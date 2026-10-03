#!/usr/bin/env sh
# Prove SQLite backups archive only the payload, never follow attacker-controlled
# paths, and take online snapshots only from the kfadapter service container.
set -eu
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

fail() {
    printf '%s\n' "backup-state-test: $*" >&2
    exit 1
}

tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/kfadapter-backup-state.XXXXXXXX")
work=$(CDPATH= cd -- "$work" && pwd -P)
cleanup() {
    rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM

actual_uid=$(id -u)
actual_gid=$(id -g)
mkdir -p "$work/bin" "$work/scripts" "$work/state"
cp "$PROJECT_ROOT/scripts/backup-state.sh" "$PROJECT_ROOT/scripts/backup-state-write.py" "$PROJECT_ROOT/scripts/docker-local-context.sh" "$PROJECT_ROOT/scripts/state-volume-path.sh" "$PROJECT_ROOT/scripts/verify-state-path.py" "$work/scripts/"
cat >"$work/scripts/preflight.sh" <<'SH'
#!/usr/bin/env sh
exit 0
SH
cat >"$work/bin/docker" <<'SH'
#!/usr/bin/env sh
case "${1:-}" in
    context)
        [ "${2:-}" = inspect ] && [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Endpoints.docker.Host}}' ] || exit 2
        printf '%s\n' "${FAKE_DOCKER_CONTEXT_HOST:-unix:///var/run/docker.sock}"
        ;;
    volume)
        [ "${2:-}" = inspect ] && [ "${3:-}" = --format ] && [ "${5:-}" = kfadapter_db_data ] || exit 2
        case "${4:-}" in
            '{{.Name}}') printf '%s\n' kfadapter_db_data ;;
            '{{.Driver}}') printf '%s\n' local ;;
            '{{.Mountpoint}}') printf '%s\n' "${FAKE_STATE_MOUNTPOINT:?}" ;;
            '{{json .Options}}') printf '%s\n' null ;;
            *) exit 2 ;;
        esac
        ;;
    ps)
        [ "${2:-}" = --filter ] && [ "${3:-}" = volume=kfadapter_db_data ] && [ "${4:-}" = -q ] || exit 2
        [ "${FAKE_DOCKER_PS_FAIL:-}" != 1 ] || exit 1
        case "${FAKE_DOCKER_RUNNING:-}" in
            1) printf '%s\n' running-container ;;
            2) printf '%s\n' running-container other-container ;;
        esac
        ;;
    inspect)
        [ "${2:-}" = --format ] && [ "${4:-}" = running-container ] && [ "$#" -eq 4 ] || exit 2
        case "${3:-}" in
            *com.docker.compose.project*com.docker.compose.service*/kfadapter/data*) ;;
            *) exit 2 ;;
        esac
        printf '%s\n' "${FAKE_CONTAINER_IDENTITY:-kfadapter/kfadapter volume:kfadapter_db_data}"
        ;;
    exec)
        [ "$#" -eq 6 ] && [ "$2" = --workdir ] && [ "$3" = /kfadapter ] && [ "$4" = running-container ] && [ "$5" = ./kfadapter ] && [ "$6" = backup ] || exit 2
        case "${FAKE_EXEC:-snapshot}" in
            snapshot) cat "${FAKE_SNAPSHOT:?}" ;;
            fail)
                printf '%s\n' 'kfadapter: backup failed: persistent state is corrupt' >&2
                exit 1
                ;;
            fail-after-output)
                cat "${FAKE_SNAPSHOT:?}"
                exit 1
                ;;
            truncated) head -c 1024 "${FAKE_SNAPSHOT:?}" ;;
            garbage) printf '%s\n' 'not a SQLite database' ;;
            wal) python3 -c 'import sys; data = bytearray(open(sys.argv[1], "rb").read()); data[18:20] = b"\x02\x02"; sys.stdout.buffer.write(data)' "${FAKE_SNAPSHOT:?}" ;;
            oversize) python3 -c 'import sys; sys.stdout.buffer.write(open(sys.argv[1], "rb").read() + b"\0" * (18 << 20))' "${FAKE_SNAPSHOT:?}" ;;
            *) exit 2 ;;
        esac
        ;;
    *) exit 2 ;;
esac
SH
chmod 0755 "$work/scripts/preflight.sh" "$work/scripts/backup-state.sh" "$work/scripts/backup-state-write.py" "$work/scripts/docker-local-context.sh" "$work/scripts/state-volume-path.sh" "$work/scripts/verify-state-path.py" "$work/bin/docker"
python3 - "$work/state/state.db" <<'PY'
import sqlite3
import sys

connection = sqlite3.connect(sys.argv[1])
connection.execute("CREATE TABLE fixture (key TEXT PRIMARY KEY, value BLOB NOT NULL)")
connection.execute("INSERT INTO fixture VALUES (?, ?)", ("backup", b"exact SQLite payload\x00"))
connection.commit()
connection.close()
PY
cp "$work/state/state.db" "$work/state.db.expected"
# The online snapshot differs from the on-disk file, proving online archives
# carry the container's stream rather than a host copy of the live file.
python3 - "$work/hot.db" <<'PY'
import sqlite3
import sys

connection = sqlite3.connect(sys.argv[1])
connection.execute("CREATE TABLE fixture (key TEXT PRIMARY KEY, value BLOB NOT NULL)")
connection.execute("INSERT INTO fixture VALUES (?, ?)", ("backup", b"online SQLite snapshot\x00"))
connection.commit()
connection.close()
PY

run_backup() {
    backup_dir=$1
    shift
    PATH="$work/bin:$PATH" \
        FAKE_DOCKER_PS_FAIL="${FAKE_DOCKER_PS_FAIL:-}" \
        FAKE_DOCKER_RUNNING="${FAKE_DOCKER_RUNNING:-}" \
        FAKE_DOCKER_CONTEXT_HOST="${FAKE_DOCKER_CONTEXT_HOST:-}" \
        FAKE_STATE_MOUNTPOINT="${FAKE_STATE_MOUNTPOINT:-$work/state}" \
        FAKE_EXEC="${FAKE_EXEC:-}" \
        FAKE_SNAPSHOT="$work/hot.db" \
        FAKE_CONTAINER_IDENTITY="${FAKE_CONTAINER_IDENTITY:-}" \
        BACKUP_DIR="$backup_dir" \
        KFADAPTER_HOST_UID="$actual_uid" \
        KFADAPTER_HOST_GID="$actual_gid" \
        "$work/scripts/backup-state.sh" "$@"
}

assert_archive_payload() {
    archive=$1
    expected=${2:-$work/state.db.expected}
    if ! python3 - "$archive" "$expected" "$actual_uid" "$actual_gid" <<'PY'
from pathlib import Path
import sys
import tarfile

archive_path, expected_path = map(Path, sys.argv[1:3])
expected_uid, expected_gid = map(int, sys.argv[3:5])
expected = expected_path.read_bytes()
if archive_path.stat().st_mode & 0o777 != 0o600:
    raise SystemExit("backup archive mode is not 0600")
with tarfile.open(archive_path, "r:gz") as archive:
    members = archive.getmembers()
    if len(members) != 1:
        raise SystemExit(f"archive has {len(members)} members instead of exactly one")
    member = members[0]
    if member.name != "state.db":
        raise SystemExit(f"archive member is {member.name!r}, not 'state.db'")
    if not member.isreg():
        raise SystemExit("archive state.db member is not regular")
    if member.mode & 0o777 != 0o600:
        raise SystemExit("archive state.db member mode is not 0600")
    if (member.uid, member.gid) != (expected_uid, expected_gid):
        raise SystemExit("archive state.db member does not record the state owner")
    payload = archive.extractfile(member)
    if payload is None or payload.read() != expected:
        raise SystemExit("archive state.db payload differs from the source bytes")
PY
    then
        fail "archive did not contain exactly the expected regular state.db payload"
    fi
}

assert_rejected_backup() {
    name=$1
    archive="$work/rejected/$name.tar.gz"
    if run_backup "$work/rejected" "$archive" >/dev/null 2>&1; then
        fail "backup accepted forbidden state entry $name"
    fi
    [ ! -e "$archive" ] || fail "rejected backup for $name created an archive"
    cmp -s "$work/state.db.expected" "$work/state/state.db" || fail "rejected backup for $name changed state.db"
}

run_backup "$work/nested/backups"
run_backup "$work/nested/backups"
set -- "$work/nested/backups"/state-*.tar.gz
[ "$#" -eq 2 ] || fail "default backups did not receive unique archive suffixes"
[ "$1" != "$2" ] || fail "backup archive suffix collided"
assert_archive_payload "$1"
assert_archive_payload "$2"

explicit="$work/explicit/archive.tar.gz"
run_backup "$work/nested/backups" "$explicit"
[ -f "$explicit" ] || fail "normal explicit archive path was not created"
assert_archive_payload "$explicit"
ps_failure_archive="$work/ps-failure/archive.tar.gz"
python3 - "$work/state/state.db" >"$work/ps-failure-state-before" <<'PY'
from pathlib import Path
import stat
import sys

metadata = Path(sys.argv[1]).lstat()
print(stat.S_IFMT(metadata.st_mode), stat.S_IMODE(metadata.st_mode), metadata.st_nlink, metadata.st_uid, metadata.st_gid, metadata.st_size, metadata.st_dev, metadata.st_ino, metadata.st_mtime_ns)
PY
if FAKE_DOCKER_PS_FAIL=1 run_backup "$work/ps-failure" "$ps_failure_archive" >"$work/ps-failure-output" 2>&1; then
    fail "backup continued after Docker ps failure"
fi
FAKE_DOCKER_PS_FAIL=
grep -Fq 'could not determine whether the Docker state volume is in use' "$work/ps-failure-output" || fail "backup did not report Docker ps failure"
[ ! -e "$ps_failure_archive" ] || fail "Docker ps failure created an archive"
cmp -s "$work/state.db.expected" "$work/state/state.db" || fail "Docker ps failure changed state.db bytes"
python3 - "$work/state/state.db" >"$work/ps-failure-state-after" <<'PY'
from pathlib import Path
import stat
import sys

metadata = Path(sys.argv[1]).lstat()
print(stat.S_IFMT(metadata.st_mode), stat.S_IMODE(metadata.st_mode), metadata.st_nlink, metadata.st_uid, metadata.st_gid, metadata.st_size, metadata.st_dev, metadata.st_ino, metadata.st_mtime_ns)
PY
cmp -s "$work/ps-failure-state-before" "$work/ps-failure-state-after" || fail "Docker ps failure changed state.db metadata"
unset FAKE_DOCKER_PS_FAIL
if FAKE_DOCKER_CONTEXT_HOST=ssh://remote.example/run/docker.sock run_backup "$work/remote-context" "$work/remote-context/archive.tar.gz" >"$work/remote-context-output" 2>&1; then
    fail "backup accepted a remote Docker context"
fi
grep -Fq 'active Docker context must use a local unix-socket endpoint' "$work/remote-context-output" || fail "backup did not report the remote Docker context"
[ ! -e "$work/remote-context/archive.tar.gz" ] || fail "remote Docker context created a backup archive"
unset FAKE_DOCKER_CONTEXT_HOST
running_archive="$work/running/archive.tar.gz"
if FAKE_DOCKER_RUNNING=1 run_backup "$work/running" --offline "$running_archive" >"$work/running-output" 2>&1; then
    fail "offline backup accepted a Docker state volume mounted by a running container"
fi
grep -Fq 'Docker state volume is in use' "$work/running-output" || fail "backup did not report the mounted state volume"
[ ! -e "$running_archive" ] || fail "mounted state volume created an archive"
cmp -s "$work/state.db.expected" "$work/state/state.db" || fail "mounted-state rejection changed state.db bytes"

# A running service is backed up online by default and on request, even while a
# transient rollback journal sits beside the live database.
online_archive="$work/online/auto.tar.gz"
run_backup "$work/online" "$online_archive" >"$work/online-output" || fail "online backup of a running service failed"
grep -Fq 'created protected online state archive' "$work/online-output" || fail "running service was not backed up online"
assert_archive_payload "$online_archive" "$work/hot.db"
printf '%s\n' transient >"$work/state/state.db-journal"
run_backup "$work/online" --online "$work/online/explicit.tar.gz" >/dev/null || fail "online backup rejected a transient journal"
rm -- "$work/state/state.db-journal"
assert_archive_payload "$work/online/explicit.tar.gz" "$work/hot.db"
run_backup "$work/online-default" >/dev/null || fail "online backup to the default directory failed"
set -- "$work/online-default"/state-*.tar.gz
[ "$#" -eq 1 ] && [ -f "$1" ] || fail "online backup did not create one default archive"
assert_archive_payload "$1" "$work/hot.db"
cmp -s "$work/state.db.expected" "$work/state/state.db" || fail "online backup changed state.db bytes"
unset FAKE_DOCKER_RUNNING

assert_rejected_online() {
    name=$1
    expected=$2
    shift 2
    archive="$work/online-rejected/$name.tar.gz"
    if run_backup "$work/online-rejected" "$@" "$archive" >"$work/online-rejected-output" 2>&1; then
        fail "online backup accepted $name"
    fi
    grep -Fq "$expected" "$work/online-rejected-output" || fail "online backup did not explain $name"
    [ ! -e "$archive" ] || fail "rejected online backup for $name created an archive"
    set -- "$work/online-rejected"/*
    [ ! -e "$1" ] || fail "rejected online backup for $name left output behind"
    cmp -s "$work/state.db.expected" "$work/state/state.db" || fail "rejected online backup for $name changed state.db"
}

assert_rejected_online stopped-service 'no running service mounts the Docker state volume' --online
FAKE_DOCKER_RUNNING=2
assert_rejected_online two-containers 'more than one container mounts the Docker state volume'
FAKE_DOCKER_RUNNING=1
FAKE_CONTAINER_IDENTITY='other/kfadapter volume:kfadapter_db_data'
assert_rejected_online foreign-project 'is not the kfadapter service'
FAKE_CONTAINER_IDENTITY='kfadapter/kfadapter bind:'
assert_rejected_online bind-mounted-state 'is not the kfadapter service'
FAKE_CONTAINER_IDENTITY=
for exec_mode in fail fail-after-output; do
    FAKE_EXEC=$exec_mode
    assert_rejected_online "exec-$exec_mode" 'could not produce a validated state snapshot'
done
FAKE_EXEC=truncated
assert_rejected_online exec-truncated 'truncated or does not match its page count'
FAKE_EXEC=garbage
assert_rejected_online exec-garbage 'not a bounded SQLite database'
FAKE_EXEC=wal
assert_rejected_online exec-wal 'does not use a rollback journal'
FAKE_EXEC=oversize
assert_rejected_online exec-oversize 'exceeds the state size limit'
FAKE_EXEC=
unset FAKE_DOCKER_RUNNING FAKE_CONTAINER_IDENTITY FAKE_EXEC
if run_backup "$work/usage" --bogus >/dev/null 2>&1; then
    fail "backup accepted an unknown option"
fi

mkdir "$work/rejected"
mv "$work/state/state.db" "$work/state.db.saved"
ln -s "$work/state.db.expected" "$work/state/state.db"
assert_rejected_backup state-db-symlink
rm -- "$work/state/state.db"
mv "$work/state.db.saved" "$work/state/state.db"

mv "$work/state/state.db" "$work/state.db.saved"
printf '%s\n' 'not a SQLite database' >"$work/state/state.db"
chmod 0600 "$work/state/state.db"
corrupt_archive="$work/rejected/corrupt-state-db.tar.gz"
if run_backup "$work/rejected" "$corrupt_archive" >/dev/null 2>&1; then
    fail "backup accepted a corrupt state.db"
fi
[ ! -e "$corrupt_archive" ] || fail "corrupt state.db backup created an archive"
rm -- "$work/state/state.db"
mv "$work/state.db.saved" "$work/state/state.db"

printf '%s\n' transient >"$work/state/state.db-wal"
assert_rejected_backup state-db-wal
rm -- "$work/state/state.db-wal"

printf '%s\n' unrelated >"$work/state/unrelated"
assert_rejected_backup unrelated-regular
rm -- "$work/state/unrelated"

printf '%s\n' symlink-target >"$work/symlink-target"
ln -s "$work/symlink-target" "$work/state/unrelated-link"
assert_rejected_backup unrelated-symlink
rm -- "$work/state/unrelated-link"

mkfifo "$work/state/unrelated-fifo"
assert_rejected_backup unrelated-fifo
rm -- "$work/state/unrelated-fifo"
if run_backup "$work/nested/backups" "$work/state/../state/forbidden.tar.gz" >/dev/null 2>&1; then
    fail "backup archive inside state passed canonical path checks"
fi
[ ! -e "$work/state/forbidden.tar.gz" ] || fail "rejected state-local archive was created"

mkdir "$work/backup-target" "$work/state-target"
printf '%s\n' sentinel >"$work/backup-target/sentinel"
printf '%s\n' state-sentinel >"$work/state-target/sentinel"
chmod 0754 "$work/backup-target" "$work/state-target"
python3 - "$work/backup-target" "$work/state-target" >"$work/targets-before" <<'PY'
from pathlib import Path
import stat
import sys
for value in sys.argv[1:]:
    metadata = Path(value).stat()
    print(value, stat.S_IMODE(metadata.st_mode), metadata.st_uid, metadata.st_gid)
PY
ln -s "$work/backup-target" "$work/backup-parent-link"
if run_backup "$work/backup-parent-link/nested" >/dev/null 2>&1; then
    fail "symlinked default backup parent passed"
fi
ln -s "$work/backup-target" "$work/explicit-parent-link"
if run_backup "$work/nested/backups" "$work/explicit-parent-link/archive.tar.gz" >/dev/null 2>&1; then
    fail "symlinked explicit backup parent passed"
fi
python3 - "$work/backup-target" "$work/state-target" >"$work/targets-after" <<'PY'
from pathlib import Path
import stat
import sys
for value in sys.argv[1:]:
    metadata = Path(value).stat()
    print(value, stat.S_IMODE(metadata.st_mode), metadata.st_uid, metadata.st_gid)
PY
cmp -s "$work/targets-before" "$work/targets-after" || fail "symlink target ownership or mode changed"
[ "$(cat "$work/backup-target/sentinel")" = sentinel ] || fail "backup output path changed symlink target contents"
[ "$(cat "$work/state-target/sentinel")" = state-sentinel ] || fail "backup state path changed symlink target contents"
[ ! -e "$work/backup-target/nested" ] || fail "symlinked default parent received output"
[ ! -e "$work/backup-target/archive.tar.gz" ] || fail "symlinked explicit parent received output"
printf '%s\n' "backup-state-test: passed"
