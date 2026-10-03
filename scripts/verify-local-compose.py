#!/usr/bin/env python3
"""Verify developer project and state isolation from rendered Compose JSON."""

import json
import sys


def verify(config: dict) -> None:
    if config.get("name") != "kfadapter-local":
        raise ValueError("developer Compose project must be kfadapter-local")
    if config.get("volumes") != {"db_data": {"name": "kfadapter-local_db_data"}}:
        raise ValueError("developer db_data must be the managed kfadapter-local_db_data volume")
    services = config.get("services", {})
    if set(services) != {"kfadapter"}:
        raise ValueError("developer Compose must contain only the kfadapter service")
    service = services["kfadapter"]
    if service.get("container_name"):
        raise ValueError("developer container names must remain project-scoped")
    mounts = service.get("volumes", [])
    state_mounts = [mount for mount in mounts if mount.get("target") == "/kfadapter/data"]
    if len(state_mounts) != 1:
        raise ValueError("developer Compose must mount exactly one state volume")
    state = state_mounts[0]
    if state.get("type") != "volume" or state.get("source") != "db_data" or state.get("read_only"):
        raise ValueError("developer state must use its writable db_data named volume")
    if any(mount.get("type") == "volume" and mount is not state for mount in mounts):
        raise ValueError("developer Compose must not mount additional named volumes")


if __name__ == "__main__":
    try:
        verify(json.load(sys.stdin))
    except (ValueError, TypeError, AttributeError) as error:
        print(f"local-compose-isolation: {error}", file=sys.stderr)
        raise SystemExit(1)
