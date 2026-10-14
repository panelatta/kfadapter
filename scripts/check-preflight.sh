#!/usr/bin/env sh
# Exercise first-run and existing managed-volume deployment preflight behavior.
set -eu
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
IMAGE_REPOSITORY=ghcr.io/panelatta/chinadapter
IMAGE_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000

fail() {
    printf '%s\n' "preflight-test: $*" >&2
    exit 1
}

tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/chinadapter-preflight.XXXXXXXX")
work=$(CDPATH= cd -- "$work" && pwd -P)
cleanup() {
    rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM

actual_uid=$(id -u)
actual_gid=$(id -g)
mkdir -p "$work/bin" "$work/scripts" "$work/state"
chmod 0700 "$work/state"
cp "$PROJECT_ROOT/scripts/docker-local-context.sh" "$PROJECT_ROOT/scripts/preflight.sh" "$PROJECT_ROOT/scripts/state-volume-path.sh" "$PROJECT_ROOT/scripts/verify-state-path.py" "$work/scripts/"
cat >"$work/bin/uname" <<'SH'
#!/usr/bin/env sh
[ "${1:-}" = -s ] || exit 2
printf '%s\n' Linux
SH
cat >"$work/bin/docker" <<'SH'
#!/usr/bin/env sh
set -eu

printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
case "${1:-}" in
    version)
        [ "${2:-}" = --format ] && [ "${3:-}" = '{{.Server.Os}}' ] || exit 2
        printf '%s\n' linux
        ;;
    info)
        [ "${2:-}" = --format ] && [ "${3:-}" = '{{.OperatingSystem}}' ] || exit 2
        printf '%s\n' 'Docker Engine - Community'
        ;;
    compose)
        shift
        case "${1:-}" in
            version)
                printf '%s\n' 'Docker Compose version v2.fixture'
                ;;
            --env-file)
                [ "${2:-}" = /dev/null ] && [ "${3:-}" = -f ] && [ "${5:-}" = config ] && [ "${6:-}" = --quiet ] || exit 2
                [ "${CHINADAPTER_IMAGE_REPOSITORY:-}" = "$FAKE_EXPECTED_IMAGE_REPOSITORY" ] || exit 2
                [ "${CHINADAPTER_IMAGE_DIGEST:-}" = "$FAKE_EXPECTED_IMAGE_DIGEST" ] || exit 2
                ;;
            *) exit 2 ;;
        esac
        ;;
    context)
        [ "${2:-}" = inspect ] && [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Endpoints.docker.Host}}' ] || exit 2
        printf '%s\n' unix:///var/run/docker.sock
        ;;
    volume)
        case "${2:-}" in
            ls)
                [ "${3:-}" = --format ] && [ "${4:-}" = '{{.Name}}' ] || exit 2
                if [ "${FAKE_LEGACY_VOLUME_EXISTS:-}" = 1 ]; then
                    printf '%s\n' kfadapter_db_data
                fi
                if [ "${FAKE_VOLUME_EXISTS:-}" = 1 ]; then
                    printf '%s\n' chinadapter_db_data
                fi
                ;;
            inspect)
                [ "${3:-}" = --format ] && [ "${5:-}" = chinadapter_db_data ] || exit 2
                [ "${FAKE_VOLUME_EXISTS:-}" = 1 ] || exit 1
                case "${4:-}" in
                    '{{.Name}}') printf '%s\n' chinadapter_db_data ;;
                    '{{.Driver}}') printf '%s\n' local ;;
                    '{{.Mountpoint}}') printf '%s\n' "${FAKE_STATE_MOUNTPOINT:?}" ;;
                    '{{json .Options}}') printf '%s\n' null ;;
                    *) exit 2 ;;
                esac
                ;;
            *) exit 2 ;;
        esac
        ;;
    *) exit 2 ;;
esac
SH
chmod 0755 "$work/scripts/docker-local-context.sh" "$work/scripts/preflight.sh" "$work/scripts/state-volume-path.sh" "$work/scripts/verify-state-path.py" "$work/bin/uname" "$work/bin/docker"

run_preflight() {
    image_repository=$1
    image_digest=$2
    (
        unset CHINADAPTER_IMAGE_REPOSITORY CHINADAPTER_IMAGE_DIGEST
        [ -z "$image_repository" ] || export CHINADAPTER_IMAGE_REPOSITORY="$image_repository"
        export CHINADAPTER_IMAGE_DIGEST="$image_digest"
        DOCKER_HOST=unix:///var/run/docker.sock \
            PATH="$work/bin:$PATH" \
            FAKE_DOCKER_LOG="$work/docker.log" \
            FAKE_VOLUME_EXISTS="${FAKE_VOLUME_EXISTS:-}" \
            FAKE_LEGACY_VOLUME_EXISTS="${FAKE_LEGACY_VOLUME_EXISTS:-}" \
            FAKE_STATE_MOUNTPOINT="${FAKE_STATE_MOUNTPOINT:-$work/state}" \
            FAKE_EXPECTED_IMAGE_REPOSITORY="$image_repository" \
            FAKE_EXPECTED_IMAGE_DIGEST="$image_digest" \
            CHINADAPTER_HOST_UID="$actual_uid" \
            CHINADAPTER_HOST_GID="$actual_gid" \
            "$work/scripts/preflight.sh"
    )
}

assert_preflight_rejected() {
    name=$1
    image_repository=$2
    image_digest=$3
    diagnostic=$4
    : >"$work/docker.log"
    if run_preflight "$image_repository" "$image_digest" >"$work/$name-output" 2>&1; then
        fail "$name image input was accepted"
    fi
    grep -Fq "$diagnostic" "$work/$name-output" || fail "$name rejection did not explain the invalid image input"
    if grep -Fq 'compose --env-file /dev/null -f ' "$work/docker.log"; then
        fail "$name invalid image input reached Compose validation"
    fi
}

: >"$work/docker.log"
run_preflight "" "$IMAGE_DIGEST" >"$work/first-run-output" 2>&1 || fail "first-run preflight rejected an absent Compose-managed db_data volume"
grep -Fqx 'preflight: passed' "$work/first-run-output" || fail "first-run preflight did not report success"
grep -Fq 'volume ls --format {{.Name}}' "$work/docker.log" || fail "first-run preflight did not check whether managed db_data exists"
if grep -Fq 'volume inspect' "$work/docker.log"; then
    fail "first-run preflight validated an absent state mountpoint"
fi
grep -Fq 'compose --env-file /dev/null -f ' "$work/docker.log" || fail "first-run preflight did not validate production Compose with project .env disabled"

: >"$work/docker.log"
run_preflight "$IMAGE_REPOSITORY" "$IMAGE_DIGEST" >"$work/override-output" 2>&1 || fail "preflight rejected an explicit untagged image repository override"
grep -Fqx 'preflight: passed' "$work/override-output" || fail "repository override preflight did not report success"
grep -Fq 'compose --env-file /dev/null -f ' "$work/docker.log" || fail "repository override preflight did not validate production Compose with project .env disabled"

: >"$work/docker.log"
FAKE_VOLUME_EXISTS=1 run_preflight "" "$IMAGE_DIGEST" >"$work/existing-output" 2>&1 || fail "preflight rejected a valid existing chinadapter_db_data volume"
grep -Fqx 'preflight: passed' "$work/existing-output" || fail "existing-volume preflight did not report success"
grep -Fq 'volume inspect --format {{.Mountpoint}} chinadapter_db_data' "$work/docker.log" || fail "existing-volume preflight did not resolve the managed mountpoint"

chmod 0755 "$work/state"
: >"$work/docker.log"
if FAKE_VOLUME_EXISTS=1 run_preflight "" "$IMAGE_DIGEST" >"$work/unsafe-output" 2>&1; then
    fail "preflight accepted an unsafe existing managed state directory"
fi
chmod 0700 "$work/state"
grep -Fq 'state directory mode must be exactly 0700' "$work/unsafe-output" || fail "unsafe existing managed state directory was not rejected"
if grep -Fq 'compose --env-file /dev/null -f ' "$work/docker.log"; then
    fail "unsafe existing state reached Compose validation"
fi

assert_preflight_rejected tagged-repository 'registry.invalid/chinadapter:latest' "$IMAGE_DIGEST" 'CHINADAPTER_IMAGE_REPOSITORY must be an untagged registry repository'
assert_preflight_rejected numeric-tag-repository 'chinadapter:5000' "$IMAGE_DIGEST" 'CHINADAPTER_IMAGE_REPOSITORY must be an untagged registry repository'
assert_preflight_rejected digest-qualified-repository "registry.invalid/chinadapter@$IMAGE_DIGEST" "$IMAGE_DIGEST" 'CHINADAPTER_IMAGE_REPOSITORY must be an untagged registry repository'
assert_preflight_rejected invalid-repository 'https://registry.invalid/chinadapter' "$IMAGE_DIGEST" 'CHINADAPTER_IMAGE_REPOSITORY must be an untagged registry repository'
assert_preflight_rejected missing-digest '' '' 'CHINADAPTER_IMAGE_DIGEST is required in production mode'
assert_preflight_rejected short-digest '' 'sha256:0' 'CHINADAPTER_IMAGE_DIGEST must be sha256:<64-lowercase-hex>'
assert_preflight_rejected uppercase-digest '' 'sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' 'CHINADAPTER_IMAGE_DIGEST must be sha256:<64-lowercase-hex>'

# Pre-rename KFADAPTER_* variables are no longer read, so every mode rejects
# them before contacting Docker instead of silently using the defaults.
: >"$work/docker.log"
if (
    export KFADAPTER_IMAGE_DIGEST="$IMAGE_DIGEST"
    run_preflight "" "$IMAGE_DIGEST"
) >"$work/legacy-env-output" 2>&1; then
    fail "production preflight accepted a renamed KFADAPTER_IMAGE_DIGEST variable"
fi
grep -Fq 'no longer read: KFADAPTER_IMAGE_DIGEST;' "$work/legacy-env-output" || fail "renamed production variable was not diagnosed"
grep -Fq 'set the matching CHINADAPTER_* variables instead' "$work/legacy-env-output" || fail "renamed production variable diagnostic did not name the replacement"
[ ! -s "$work/docker.log" ] || fail "renamed production variable reached Docker"

run_state_only() {
    PATH="$work/bin:$PATH" \
        FAKE_DOCKER_LOG="$work/docker.log" \
        CHINADAPTER_HOST_UID="$actual_uid" \
        CHINADAPTER_HOST_GID="$actual_gid" \
        "$work/scripts/preflight.sh" --state-only --state-dir "$work/state"
}
run_state_only >"$work/state-only-output" 2>&1 || fail "state-only preflight rejected a valid empty state directory"
if (
    export KFADAPTER_STATE_DIR="$work/state" KFADAPTER_HOST_UID="$actual_uid"
    run_state_only
) >"$work/legacy-state-env-output" 2>&1; then
    fail "state-only preflight accepted renamed KFADAPTER_* variables"
fi
grep -Fq 'no longer read: KFADAPTER_HOST_UID KFADAPTER_STATE_DIR;' "$work/legacy-state-env-output" || fail "renamed state variables were not all diagnosed"

# A host deployed before the rename still holds its state in kfadapter_db_data;
# production preflight must send it to the migration instead of a first run.
: >"$work/docker.log"
if FAKE_VOLUME_EXISTS='' FAKE_LEGACY_VOLUME_EXISTS=1 run_preflight "" "$IMAGE_DIGEST" >"$work/legacy-volume-output" 2>&1; then
    fail "preflight accepted a host that has only the pre-rename state volume"
fi
unset FAKE_VOLUME_EXISTS FAKE_LEGACY_VOLUME_EXISTS
grep -Fq 'found the pre-rename kfadapter_db_data state volume but no chinadapter_db_data volume' "$work/legacy-volume-output" ||
    fail "unmigrated legacy state volume was not diagnosed"
grep -Fq 'sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY,CHINADAPTER_IMAGE_DIGEST scripts/migrate-legacy-state.sh' "$work/legacy-volume-output" ||
    fail "unmigrated legacy state volume diagnostic did not name the migration command"
if grep -Fq 'compose --env-file /dev/null -f ' "$work/docker.log"; then
    fail "unmigrated legacy state volume reached Compose validation"
fi

: >"$work/docker.log"
FAKE_VOLUME_EXISTS=1 FAKE_LEGACY_VOLUME_EXISTS=1 run_preflight "" "$IMAGE_DIGEST" >"$work/retained-legacy-output" 2>&1 ||
    fail "preflight rejected a migrated host that retains the pre-rename volume"
unset FAKE_VOLUME_EXISTS FAKE_LEGACY_VOLUME_EXISTS
grep -Fqx 'preflight: passed' "$work/retained-legacy-output" || fail "migrated host preflight did not report success"
grep -Fq 'volume inspect --format {{.Mountpoint}} chinadapter_db_data' "$work/docker.log" || fail "migrated host preflight did not validate chinadapter_db_data"

printf '%s\n' "preflight-test: passed"
