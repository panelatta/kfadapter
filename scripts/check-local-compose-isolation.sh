#!/usr/bin/env sh
# Read-only Compose rendering plus fake-Docker regression coverage; never starts a container.
set -eu
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
work=$(mktemp -d "${TMPDIR:-/tmp}/kfadapter-local-isolation.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT HUP INT TERM

fail() {
    printf '%s\n' "local-compose-isolation-test: $*" >&2
    exit 1
}

mkdir -p "$work/deploy" "$work/scripts" "$work/bin"
cp "$PROJECT_ROOT/deploy/compose.local-build.yaml" "$work/deploy/"
cp "$SCRIPT_DIR/preflight.sh" "$SCRIPT_DIR/verify-local-compose.py" "$SCRIPT_DIR/check-local-compose-first-run.sh" "$work/scripts/"
printf '%s\n' '{}' >"$work/config.yaml"
printf '%s\n' 'COMPOSE_PROJECT_NAME=kfadapter' 'KFADAPTER_LOCAL_IMAGE=wrong:dotenv' >"$work/.env"
cp "$work/.env" "$work/deploy/.env"

# Compose config needs only the CLI, not a running daemon. Verify both the
# colliding inherited name and .env are overridden, while the image is retained.
for inherited_project in kfadapter another-project; do
    COMPOSE_PROJECT_NAME=$inherited_project KFADAPTER_LOCAL_IMAGE=kfadapter:test-override \
        docker compose --env-file /dev/null --project-name kfadapter-local \
        -f "$work/deploy/compose.local-build.yaml" config --format json >"$work/valid.json"
    python3 "$SCRIPT_DIR/verify-local-compose.py" <"$work/valid.json"
    python3 - "$work/valid.json" <<'PY'
import json
import sys
with open(sys.argv[1]) as source:
    config = json.load(source)
assert config["services"]["kfadapter"]["image"] == "kfadapter:test-override"
PY
done
(
    unset KFADAPTER_LOCAL_IMAGE
    COMPOSE_PROJECT_NAME=kfadapter docker compose --env-file /dev/null --project-name kfadapter-local \
        -f "$work/deploy/compose.local-build.yaml" config --format json >"$work/default.json"
)
python3 "$SCRIPT_DIR/verify-local-compose.py" <"$work/default.json"
python3 - "$work/default.json" <<'PY'
import json
import sys
with open(sys.argv[1]) as source:
    config = json.load(source)
assert config["services"]["kfadapter"]["image"] == "kfadapter:local"
PY

cat >"$work/bin/uname" <<'SH'
#!/usr/bin/env sh
[ "${1:-}" = -s ] || exit 2
printf '%s\n' Linux
SH
cat >"$work/bin/docker" <<'PY'
#!/usr/bin/env python3
import json
import os
import sys

args = sys.argv[1:]
with open(os.environ["FAKE_DOCKER_LOG"], "a") as log:
    log.write(json.dumps({"args": args, "image": os.environ.get("KFADAPTER_LOCAL_IMAGE")}) + "\n")
if args == ["version", "--format", "{{.Server.Os}}"]:
    print("linux")
elif args == ["info", "--format", "{{.OperatingSystem}}"]:
    print("Docker Engine - Community")
elif args == ["compose", "version"]:
    print("Docker Compose version v2.fixture")
elif args[:2] == ["image", "inspect"]:
    pass
elif args[:5] == ["compose", "--env-file", "/dev/null", "--project-name", "kfadapter-local"] and args[5] == "-f":
    operation = args[7:]
    if operation == ["config", "--format", "json"]:
        with open(os.environ["FAKE_CONFIG"]) as config:
            print(config.read())
    elif operation[:1] == ["down"]:
        pass
    elif operation[:1] == ["up"]:
        sys.exit("simulated stop before startup")
    else:
        sys.exit("unexpected fixture Compose operation")
else:
    sys.exit("unexpected fixture Docker command")
PY
chmod 0755 "$work/bin/uname" "$work/bin/docker"

fake_command() {
    (
        unset KFADAPTER_STATE_DIR STATE_DIR
        PATH="$work/bin:$PATH" COMPOSE_PROJECT_NAME=kfadapter \
            KFADAPTER_LOCAL_IMAGE=kfadapter:test-override \
            FAKE_DOCKER_LOG="$work/docker.log" FAKE_CONFIG="$FAKE_CONFIG" "$@"
    )
}

FAKE_CONFIG=$work/valid.json
: >"$work/docker.log"
fake_command sh "$work/scripts/preflight.sh" --local-build >"$work/output" 2>&1 ||
    fail "local preflight rejected the pinned project under a production environment override"
grep -Fqx 'preflight: passed' "$work/output" || fail "local preflight did not pass"
if fake_command sh "$work/scripts/check-local-compose-first-run.sh" kfadapter:test-override >"$work/output" 2>&1; then
    fail "first-run fixture did not stop at the simulated startup boundary"
fi
grep -Fq 'simulated stop before startup' "$work/output" || fail "first-run fixture failed before isolation was checked"
python3 - "$work/docker.log" <<'PY'
import json
import sys
with open(sys.argv[1]) as source:
    calls = [json.loads(line) for line in source]
operations = []
for call in calls:
    args = call["args"]
    if args[0] != "compose" or args[1:] == ["version"]:
        continue
    assert args[:5] == ["compose", "--env-file", "/dev/null", "--project-name", "kfadapter-local"]
    assert call["image"] == "kfadapter:test-override"
    operations.append(args[7])
assert operations == ["config", "config", "down", "up", "down"], operations
PY

# Check the rendered contract itself, independently of the command's flags.
python3 - "$work" <<'PY'
import copy
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
config = json.loads((root / "valid.json").read_text())
cases = {}
value = copy.deepcopy(config)
value["name"] = "kfadapter"
cases["production-project"] = value
value = copy.deepcopy(config)
value["volumes"]["db_data"]["name"] = "kfadapter_db_data"
cases["production-volume"] = value
value = copy.deepcopy(config)
value["volumes"]["db_data"]["external"] = True
cases["external-volume"] = value
value = copy.deepcopy(config)
state = next(m for m in value["services"]["kfadapter"]["volumes"] if m["target"] == "/kfadapter/data")
state.update(type="bind", source="/production-state")
cases["bind-state"] = value
value = copy.deepcopy(config)
value["services"]["kfadapter"]["volumes"].append({"type": "volume", "source": "kfadapter_db_data", "target": "/other"})
cases["extra-volume"] = value
value = copy.deepcopy(config)
value["services"]["kfadapter"]["container_name"] = "kfadapter"
cases["unscoped-container"] = value
for name, value in cases.items():
    (root / f"invalid-{name}.json").write_text(json.dumps(value))
PY
for FAKE_CONFIG in "$work"/invalid-*.json; do
    : >"$work/docker.log"
    if fake_command sh "$work/scripts/preflight.sh" --local-build >"$work/output" 2>&1; then
        fail "local preflight accepted $FAKE_CONFIG"
    fi
    grep -Fq 'local-compose-isolation:' "$work/output" || fail "invalid rendered configuration was not diagnosed"
    if fake_command sh "$work/scripts/check-local-compose-first-run.sh" kfadapter:test-override >"$work/output" 2>&1; then
        fail "first-run check accepted $FAKE_CONFIG"
    fi
    python3 - "$work/docker.log" <<'PY'
import json
import sys
with open(sys.argv[1]) as source:
    for line in source:
        args = json.loads(line)["args"]
        assert "up" not in args and "down" not in args, args
PY
done

printf '%s\n' "local-compose-isolation-test: passed"
