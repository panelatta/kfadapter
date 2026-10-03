#!/usr/bin/env sh
# Render a custom configuration mount and require the health probe to use it.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000
unset KFADAPTER_IMAGE_REPOSITORY KFADAPTER_IMAGE

fail() {
    printf '%s\n' "configured-healthcheck: $*" >&2
    exit 1
}

tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/kfadapter-healthcheck.XXXXXXXX")
work=$(CDPATH= cd -- "$work" && pwd -P)
cleanup() {
    rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM

config="$work/config.yaml"
override="$work/compose-health.yaml"
cat >"$config" <<'YAML'
listenAddr: 127.0.0.1
management:
  port: 12009
  sessionTTL: 30m
proxy:
  port: 12008
  dialTimeout: 10s
  handshakeTimeout: 15s
provider:
  requestTimeout: 15s
  refreshInterval: 2h
YAML
cat >"$override" <<COMPOSE
services:
  kfadapter:
    volumes:
      - type: bind
        source: $config
        target: /kfadapter/config.yaml
        read_only: true
        bind:
          create_host_path: false
COMPOSE

KFADAPTER_IMAGE_DIGEST="$IMAGE_DIGEST" \
docker compose --env-file /dev/null -f "$PROJECT_ROOT/compose.yaml" -f "$override" config --format json >"$work/rendered.json" ||
    fail "docker compose config failed"
python3 - "$config" "$work/rendered.json" <<'PY'
from pathlib import Path
import json
import sys

config = Path(sys.argv[1]).resolve()
rendered = json.loads(Path(sys.argv[2]).read_text())
service = rendered["services"]["kfadapter"]
assert service["working_dir"] == "/kfadapter"
assert service["image"] == "ghcr.io/oshinop/kfadapter@sha256:0000000000000000000000000000000000000000000000000000000000000000"
# The probe takes no arguments: it reads ./config.yaml from the working
# directory, so a custom mount changes the probed ports without a new command.
assert service["healthcheck"]["test"] == ["CMD", "./kfadapter", "healthcheck"]
assert service["network_mode"] == "host"
assert not service.get("ports")
mounts = {mount["target"]: mount for mount in service["volumes"]}
state = mounts["/kfadapter/data"]
config_mount = mounts["/kfadapter/config.yaml"]
assert state["type"] == "volume"
assert state["source"] == "db_data"
assert state.get("read_only") is not True
assert config_mount["type"] == "bind"
assert config_mount["read_only"] is True
assert Path(config_mount["source"]).resolve() == config
assert rendered["volumes"] == {"db_data": {"name": "kfadapter_db_data"}}
PY
printf '%s\n' "configured-healthcheck: passed"
