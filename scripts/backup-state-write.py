#!/usr/bin/env python3
"""Create a state backup through verified directory descriptors only.

Offline backups copy the stopped service's state.db. Online backups archive the
validated snapshot that `kfadapter backup` streams from the running container.
"""

from __future__ import annotations

import argparse
import datetime as dt
import os
import secrets
import io
import stat
import subprocess
import sys
import tarfile
import time
from pathlib import Path

STATE_FILE_NAME = "state.db"
MAX_STATE_BYTES = 18 << 20
SQLITE_HEADER = b"SQLite format 3\x00"
CHUNK_SIZE = 1024 * 1024
CONTAINER_WORKDIR = "/kfadapter"


class BackupError(Exception):
    pass


def fail(message: str) -> None:
    raise BackupError(message)


def absolute_parts(path: str, base: Path, option: str) -> list[str]:
    candidate = Path(path)
    if not candidate.is_absolute():
        candidate = base / candidate
    if not candidate.is_absolute():
        fail(f"{option} must resolve to an absolute path")
    parts = list(candidate.parts)
    if len(parts) < 2 or any(part in {"", ".", ".."} for part in parts[1:]):
        fail(f"{option} must not contain noncanonical path components")
    return parts[1:]


def open_verified_directory(parts: list[str], create_missing: bool) -> int:
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open("/", flags)
    try:
        for part in parts:
            try:
                metadata = os.stat(part, dir_fd=descriptor, follow_symlinks=False)
            except FileNotFoundError:
                if not create_missing:
                    fail("backup archive parent does not exist")
                try:
                    os.mkdir(part, 0o700, dir_fd=descriptor)
                except FileExistsError:
                    pass
                except OSError as error:
                    fail(f"could not create backup archive parent: {error}")
                metadata = os.stat(part, dir_fd=descriptor, follow_symlinks=False)
            except OSError as error:
                fail(f"could not inspect backup archive parent: {error}")
            if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode):
                fail("backup archive parent must contain only non-symlink directories")
            try:
                child = os.open(part, flags, dir_fd=descriptor)
            except OSError as error:
                fail(f"could not open backup archive parent safely: {error}")
            os.close(descriptor)
            descriptor = child
        return descriptor
    except Exception:
        os.close(descriptor)
        raise


def state_directory(path: str, base: Path, expected_identity: str) -> int:
    descriptor = open_verified_directory(absolute_parts(path, base, "--state-dir"), create_missing=False)
    metadata = os.fstat(descriptor)
    if f"{metadata.st_dev}:{metadata.st_ino}" != expected_identity:
        os.close(descriptor)
        fail("state directory identity changed")
    return descriptor


def is_same_or_descendant(directory_fd: int, possible_ancestor_fd: int) -> bool:
    ancestor = os.fstat(possible_ancestor_fd)
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    current = os.dup(directory_fd)
    try:
        while True:
            metadata = os.fstat(current)
            if (metadata.st_dev, metadata.st_ino) == (ancestor.st_dev, ancestor.st_ino):
                return True
            parent = os.open("..", flags, dir_fd=current)
            parent_metadata = os.fstat(parent)
            if (parent_metadata.st_dev, parent_metadata.st_ino) == (metadata.st_dev, metadata.st_ino):
                os.close(parent)
                return False
            os.close(current)
            current = parent
    finally:
        os.close(current)


def reserve_output(parent_fd: int, requested_name: str | None) -> tuple[int, str]:
    if requested_name is not None:
        names = [requested_name]
    else:
        timestamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        names = [f"state-{timestamp}-{secrets.token_hex(16)}.tar.gz" for _ in range(32)]
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    for name in names:
        try:
            return os.open(name, flags, 0o600, dir_fd=parent_fd), name
        except FileExistsError:
            continue
        except OSError as error:
            fail(f"could not reserve backup archive path: {error}")
    fail("could not reserve a unique backup archive path")


def same_file_identity(first: os.stat_result, second: os.stat_result) -> bool:
    return (first.st_dev, first.st_ino, stat.S_IFMT(first.st_mode)) == (
        second.st_dev,
        second.st_ino,
        stat.S_IFMT(second.st_mode),
    )


def open_state_file(state_fd: int) -> tuple[int, os.stat_result]:
    entries = list(os.scandir(state_fd))
    if len(entries) != 1 or entries[0].name != STATE_FILE_NAME:
        fail("state directory must contain exactly one state.db file")
    entry = entries[0]
    metadata = entry.stat(follow_symlinks=False)
    if (
        stat.S_ISLNK(metadata.st_mode)
        or not stat.S_ISREG(metadata.st_mode)
        or metadata.st_nlink != 1
        or metadata.st_size <= 0
        or metadata.st_size > MAX_STATE_BYTES
        or stat.S_IMODE(metadata.st_mode) != 0o600
    ):
        fail("state.db must be a non-empty 0600 regular file with one link")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        descriptor = os.open(STATE_FILE_NAME, flags, dir_fd=state_fd)
    except OSError as error:
        fail(f"could not open state.db safely: {error}")
    try:
        opened = os.fstat(descriptor)
        if (
            not same_file_identity(metadata, opened)
            or not stat.S_ISREG(opened.st_mode)
            or opened.st_nlink != 1
            or opened.st_size != metadata.st_size
            or opened.st_size > MAX_STATE_BYTES
            or stat.S_IMODE(opened.st_mode) != 0o600
        ):
            fail("state.db identity changed")
        header = os.read(descriptor, len(SQLITE_HEADER))
        if header != SQLITE_HEADER:
            fail("state.db does not have a SQLite database header")
        os.lseek(descriptor, 0, os.SEEK_SET)
        return descriptor, opened
    except Exception:
        os.close(descriptor)
        raise


def verify_state_file(state_fd: int, expected: os.stat_result) -> None:
    try:
        final = os.stat(STATE_FILE_NAME, dir_fd=state_fd, follow_symlinks=False)
    except OSError as error:
        fail(f"could not recheck state.db safely: {error}")
    if (
        not same_file_identity(expected, final)
        or not stat.S_ISREG(final.st_mode)
        or final.st_nlink != 1
        or final.st_size != expected.st_size
        or final.st_size > MAX_STATE_BYTES
        or stat.S_IMODE(final.st_mode) != 0o600
    ):
        fail("state.db identity changed")


def stream_tar(source, size: int, uid: int, gid: int, mtime: int, output_fd: int) -> None:
    info = tarfile.TarInfo(STATE_FILE_NAME)
    info.size = size
    info.mode = 0o600
    info.uid = uid
    info.gid = gid
    info.mtime = mtime
    try:
        with os.fdopen(os.dup(output_fd), "wb", closefd=True) as output:
            with tarfile.open(fileobj=output, mode="w:gz", format=tarfile.USTAR_FORMAT) as archive:
                archive.addfile(info, source)
    except (OSError, tarfile.TarError) as error:
        fail(f"could not create state archive: {error}")


def validate_snapshot(image: bytes) -> None:
    """Accept only a complete rollback-journal database, as restore requires."""
    if len(image) < 100 or len(image) > MAX_STATE_BYTES or not image.startswith(SQLITE_HEADER):
        fail("online snapshot is not a bounded SQLite database")
    if image[18] != 1 or image[19] != 1:
        fail("online snapshot does not use a rollback journal")
    page_size = int.from_bytes(image[16:18], "big")
    if page_size == 1:
        page_size = 65536
    pages = int.from_bytes(image[28:32], "big")
    if page_size < 512 or page_size & (page_size - 1) or pages * page_size != len(image):
        fail("online snapshot is truncated or does not match its page count")


def read_online_snapshot(container: str) -> bytes:
    """Run `kfadapter backup` in the service container and bound its output."""
    command = ["docker", "exec", "--workdir", CONTAINER_WORKDIR, container, "./kfadapter", "backup"]
    try:
        process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE)
    except OSError as error:
        fail(f"could not run the online snapshot: {error}")
    chunks: list[bytes] = []
    total = 0
    try:
        while True:
            chunk = process.stdout.read(CHUNK_SIZE)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_STATE_BYTES:
                fail("online snapshot exceeds the state size limit")
            chunks.append(chunk)
    finally:
        if total > MAX_STATE_BYTES:
            process.kill()
        process.stdout.close()
        returncode = process.wait()
    if returncode != 0:
        fail("the running service could not produce a validated state snapshot")
    image = b"".join(chunks)
    validate_snapshot(image)
    return image


def write_archive(parent_fd: int, archive_name: str | None, write, recheck=None) -> str:
    output_fd, output_name = reserve_output(parent_fd, archive_name)
    try:
        write(output_fd)
        if recheck is not None:
            recheck()
        os.fchmod(output_fd, 0o600)
        os.fsync(output_fd)
        os.fsync(parent_fd)
    except Exception:
        try:
            os.close(output_fd)
        finally:
            os.unlink(output_name, dir_fd=parent_fd)
            os.fsync(parent_fd)
        raise
    os.close(output_fd)
    return output_name


def create_backup(
    project_root: Path,
    state_path: str,
    state_identity: str | None,
    destination_path: str,
    archive_path: str | None,
    online_container: str | None,
    owner: tuple[int, int] | None,
) -> None:
    if archive_path is not None:
        archive_parts = absolute_parts(archive_path, project_root, "archive path")
        archive_name = archive_parts.pop()
        if not archive_name or archive_name in {".", ".."}:
            fail("archive path must name a regular file")
    else:
        archive_parts = absolute_parts(destination_path, project_root, "--backup-dir")
        archive_name = None

    parent_fd = open_verified_directory(archive_parts, create_missing=True)
    if online_container is not None:
        # The container validates state itself; the host directory is opened
        # only to keep the archive out of it, so a transient journal beside a
        # live state.db is not an error here.
        state_fd = open_verified_directory(absolute_parts(state_path, project_root, "--state-dir"), create_missing=False)
    else:
        state_fd = state_directory(state_path, project_root, state_identity)
    state_file_fd = -1
    try:
        if is_same_or_descendant(parent_fd, state_fd):
            fail("backup archive must not be placed inside state")
        if online_container is not None:
            image = read_online_snapshot(online_container)
            uid, gid = owner
            output_name = write_archive(
                parent_fd,
                archive_name,
                lambda output_fd: stream_tar(io.BytesIO(image), len(image), uid, gid, int(time.time()), output_fd),
            )
        else:
            state_file_fd, state_metadata = open_state_file(state_fd)

            def write(output_fd: int) -> None:
                with os.fdopen(os.dup(state_file_fd), "rb", closefd=True) as source:
                    stream_tar(
                        source,
                        state_metadata.st_size,
                        state_metadata.st_uid,
                        state_metadata.st_gid,
                        int(state_metadata.st_mtime),
                        output_fd,
                    )

            output_name = write_archive(parent_fd, archive_name, write, lambda: verify_state_file(state_fd, state_metadata))
        print(output_name)
    finally:
        if state_file_fd >= 0:
            os.close(state_file_fd)
        os.close(state_fd)
        os.close(parent_fd)


def owner_id(value: str, option: str) -> int:
    if not value.isdecimal() or len(value) > 10:
        fail(f"{option} must be a decimal numeric id")
    return int(value)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-root", required=True, type=Path)
    parser.add_argument("--state-dir", required=True)
    parser.add_argument("--state-identity")
    parser.add_argument("--online-container")
    parser.add_argument("--uid")
    parser.add_argument("--gid")
    parser.add_argument("--backup-dir", required=True)
    parser.add_argument("--archive")
    args = parser.parse_args()
    if not args.project_root.is_absolute() or args.project_root.is_symlink():
        fail("--project-root must be an absolute non-symlink directory")
    owner = None
    if args.online_container is not None:
        if args.state_identity is not None or args.uid is None or args.gid is None:
            fail("--online-container requires --uid and --gid and excludes --state-identity")
        if not args.online_container or args.online_container.startswith("-"):
            fail("--online-container must name a container")
        owner = (owner_id(args.uid, "--uid"), owner_id(args.gid, "--gid"))
    elif args.state_identity is None or args.uid is not None or args.gid is not None:
        fail("an offline backup requires --state-identity and takes its owner from state.db")
    create_backup(
        args.project_root,
        args.state_dir,
        args.state_identity,
        args.backup_dir,
        args.archive,
        args.online_container,
        owner,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except BackupError as error:
        print(f"backup-state: {error}", file=sys.stderr)
        raise SystemExit(1)
