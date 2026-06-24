#!/usr/bin/env python3
#
# Copyright 2026 The Forgejo Authors. All rights reserved.
# SPDX-License-Identifier: MIT
"""Localize GitHub-hosted image assets referenced from Forgejo markdown.

This is a one-shot/resumable maintenance tool for GitHub metadata mirrors. It
downloads GitHub-hosted image URLs found in SQLite-backed issue/comment/review
content, stores them as normal Forgejo attachments, and rewrites the markdown to
point at the local attachment URL.
"""

from __future__ import annotations

import argparse
import collections
import contextlib
import hashlib
import html
import mimetypes
import os
import re
import sqlite3
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass, field
from pathlib import Path


GHOST_USER_ID = -1
COMMENT_TYPE_REVIEW = 22

URL_RE = re.compile(
    r"https://(?:"
    r"user-images\.githubusercontent\.com"
    r"|private-user-images\.githubusercontent\.com"
    r"|github\.com/[^\s\]\[()\"'<>]+/(?:assets|user-attachments/assets)/[^\s\]\[()\"'<>]+"
    r"|github-production-user-asset-[A-Za-z0-9-]+\.s3\.amazonaws\.com"
    r")[^\s\]\[()\"'<>]*"
)

IMAGE_EXTENSIONS = {
    ".apng",
    ".avif",
    ".bmp",
    ".gif",
    ".jpg",
    ".jpeg",
    ".jxl",
    ".png",
    ".svg",
    ".webp",
}

EXTENSION_BY_CONTENT_TYPE = {
    "image/jpeg": ".jpg",
    "image/svg+xml": ".svg",
}


@dataclass(frozen=True)
class Context:
    repo_id: int
    issue_id: int = 0
    comment_id: int = 0
    release_id: int = 0
    uploader_id: int = GHOST_USER_ID
    created_unix: int = 0


@dataclass
class ContentRow:
    table: str
    column: str
    row_id: int
    content: str
    context: Context


@dataclass
class Asset:
    repo_id: int
    canonical_url: str
    fetch_url: str
    exact_urls: set[str] = field(default_factory=set)
    context: Context | None = None
    local_url: str | None = None
    error: str | None = None
    skipped: str | None = None


@dataclass
class DownloadedAsset:
    path: Path
    size: int
    content_type: str
    final_url: str


def main() -> int:
    args = parse_args()
    db_path = args.db.resolve()
    attachments_dir = args.attachments_dir.resolve() if args.attachments_dir else db_path.parent / "attachments"
    local_url_base = args.local_url_base
    if not local_url_base.endswith("/"):
        local_url_base += "/"

    conn = connect(db_path, readonly=args.dry_run, busy_timeout=args.busy_timeout)
    try:
        rows = scan_content_rows(conn, args.repo_id)
        assets = collect_assets(rows)

        print(f"Scanned {len(rows)} content rows")
        print(f"Found {sum(len(row_exact_urls(row.content)) for row in rows)} GitHub asset URL occurrences")
        print(f"Found {len(assets)} distinct canonical asset URLs")

        if args.dry_run:
            print("Dry run: no backup, downloads, attachment writes, or DB updates were performed")
            return 0

        if not args.no_backup:
            backup_path = args.backup_path or default_backup_path(db_path)
            backup_database(conn, backup_path)
            print(f"Backed up database to {backup_path}")

        owner = storage_owner(attachments_dir)
        stats = localize_assets(conn, assets, attachments_dir, local_url_base, owner, args)
        update_stats = rewrite_rows(conn, rows, assets)

        print(
            "Localized assets: "
            f"{stats['created']} created, "
            f"{stats['reused']} reused, "
            f"{stats['refetched']} refetched, "
            f"{stats['skipped']} skipped, "
            f"{stats['failed']} failed"
        )
        print(
            "Rewrote content rows: "
            f"{update_stats['updated']} updated, "
            f"{update_stats['unchanged']} unchanged, "
            f"{update_stats['conflicted']} skipped after concurrent change"
        )

        failures = [asset for asset in assets.values() if asset.error]
        if failures:
            print("\nFailed assets:", file=sys.stderr)
            for asset in failures[:20]:
                print(f"- {asset.fetch_url}: {asset.error}", file=sys.stderr)
            if len(failures) > 20:
                print(f"- ... {len(failures) - 20} more", file=sys.stderr)
            return 1

        return 0
    finally:
        conn.close()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Download GitHub-hosted markdown images into Forgejo attachment storage and rewrite SQLite content rows.",
    )
    parser.add_argument("--db", type=Path, required=True, help="Path to Forgejo's SQLite database.")
    parser.add_argument(
        "--attachments-dir",
        type=Path,
        help="Path to Forgejo attachment storage. Defaults to <db-dir>/attachments.",
    )
    parser.add_argument(
        "--local-url-base",
        default="/attachments/",
        help="URL prefix written into markdown. Defaults to /attachments/.",
    )
    parser.add_argument("--repo-id", type=int, help="Only rewrite one repository ID.")
    parser.add_argument("--backup-path", type=Path, help="Explicit database backup path.")
    parser.add_argument("--no-backup", action="store_true", help="Do not create an online SQLite backup before writing.")
    parser.add_argument("--dry-run", action="store_true", help="Only scan and report candidate URLs.")
    parser.add_argument("--busy-timeout", type=float, default=30.0, help="SQLite busy timeout in seconds.")
    parser.add_argument("--http-timeout", type=float, default=30.0, help="HTTP request timeout in seconds.")
    parser.add_argument("--max-bytes", type=int, default=50 * 1024 * 1024, help="Maximum image size to download.")
    parser.add_argument("--limit-assets", type=int, help="Only process the first N distinct assets.")
    parser.add_argument(
        "--tmp-dir",
        type=Path,
        help="Temporary download directory. Defaults to <attachments-dir>/tmp.",
    )
    parser.add_argument(
        "--user-agent",
        default="Forgejo GitHub asset localizer",
        help="User-Agent used for image downloads.",
    )
    return parser.parse_args()


def connect(db_path: Path, readonly: bool, busy_timeout: float) -> sqlite3.Connection:
    if readonly:
        uri = f"file:{urllib.parse.quote(str(db_path))}?mode=ro"
        conn = sqlite3.connect(uri, uri=True, timeout=busy_timeout)
    else:
        conn = sqlite3.connect(db_path, timeout=busy_timeout)
    conn.row_factory = sqlite3.Row
    conn.execute(f"PRAGMA busy_timeout = {int(busy_timeout * 1000)}")
    return conn


def scan_content_rows(conn: sqlite3.Connection, repo_id: int | None) -> list[ContentRow]:
    rows: list[ContentRow] = []
    params: list[int] = []
    repo_filter = ""
    if repo_id is not None:
        repo_filter = " AND repo_id = ?"
        params.append(repo_id)

    for row in conn.execute(
        f"""
        SELECT id, repo_id, poster_id, created_unix, content
        FROM issue
        WHERE content IS NOT NULL AND content != ''{repo_filter}
        """,
        params,
    ):
        rows.append(
            ContentRow(
                table="issue",
                column="content",
                row_id=row["id"],
                content=row["content"],
                context=Context(
                    repo_id=row["repo_id"],
                    issue_id=row["id"],
                    uploader_id=valid_uploader(row["poster_id"]),
                    created_unix=row["created_unix"] or 0,
                ),
            )
        )

    params = []
    repo_filter = ""
    if repo_id is not None:
        repo_filter = " AND issue.repo_id = ?"
        params.append(repo_id)
    for row in conn.execute(
        f"""
        SELECT comment.id, issue.repo_id, comment.issue_id, comment.poster_id,
               comment.created_unix, comment.content
        FROM comment
        INNER JOIN issue ON issue.id = comment.issue_id
        WHERE comment.content IS NOT NULL AND comment.content != ''{repo_filter}
        """,
        params,
    ):
        rows.append(
            ContentRow(
                table="comment",
                column="content",
                row_id=row["id"],
                content=row["content"],
                context=Context(
                    repo_id=row["repo_id"],
                    issue_id=row["issue_id"],
                    comment_id=row["id"],
                    uploader_id=valid_uploader(row["poster_id"]),
                    created_unix=row["created_unix"] or 0,
                ),
            )
        )

    params = []
    repo_filter = ""
    if repo_id is not None:
        repo_filter = " AND issue.repo_id = ?"
        params.append(repo_id)
    for row in conn.execute(
        f"""
        SELECT review.id, issue.repo_id, review.issue_id, review.reviewer_id,
               review.created_unix, review.content, comment.id AS comment_id
        FROM review
        INNER JOIN issue ON issue.id = review.issue_id
        LEFT JOIN comment
          ON comment.review_id = review.id
         AND comment.type = {COMMENT_TYPE_REVIEW}
        WHERE review.content IS NOT NULL AND review.content != ''{repo_filter}
        """,
        params,
    ):
        rows.append(
            ContentRow(
                table="review",
                column="content",
                row_id=row["id"],
                content=row["content"],
                context=Context(
                    repo_id=row["repo_id"],
                    issue_id=row["issue_id"],
                    comment_id=row["comment_id"] or 0,
                    uploader_id=valid_uploader(row["reviewer_id"]),
                    created_unix=row["created_unix"] or 0,
                ),
            )
        )

    params = []
    repo_filter = ""
    if repo_id is not None:
        repo_filter = " AND repo_id = ?"
        params.append(repo_id)
    for row in conn.execute(
        f"""
        SELECT id, repo_id, publisher_id, created_unix, note
        FROM release
        WHERE note IS NOT NULL AND note != ''{repo_filter}
        """,
        params,
    ):
        rows.append(
            ContentRow(
                table="release",
                column="note",
                row_id=row["id"],
                content=row["note"],
                context=Context(
                    repo_id=row["repo_id"],
                    release_id=row["id"],
                    uploader_id=valid_uploader(row["publisher_id"]),
                    created_unix=row["created_unix"] or 0,
                ),
            )
        )

    return [row for row in rows if row_exact_urls(row.content)]


def valid_uploader(value: int | None) -> int:
    return value if value else GHOST_USER_ID


def row_exact_urls(content: str) -> list[str]:
    urls: list[str] = []
    for match in URL_RE.finditer(content):
        url = match.group(0).rstrip(".,;:")
        if url:
            urls.append(url)
    return urls


def collect_assets(rows: list[ContentRow]) -> collections.OrderedDict[tuple[int, str], Asset]:
    assets: collections.OrderedDict[tuple[int, str], Asset] = collections.OrderedDict()
    for row in rows:
        for exact_url in row_exact_urls(row.content):
            canonical = canonicalize_url(exact_url)
            key = (row.context.repo_id, canonical)
            asset = assets.get(key)
            if asset is None:
                asset = Asset(
                    repo_id=row.context.repo_id,
                    canonical_url=canonical,
                    fetch_url=html.unescape(exact_url),
                    context=row.context,
                )
                assets[key] = asset
            asset.exact_urls.add(exact_url)
            if asset.context is None:
                asset.context = row.context
    return assets


def canonicalize_url(url: str) -> str:
    parsed = urllib.parse.urlsplit(html.unescape(url))
    path = urllib.parse.unquote(parsed.path)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc.lower(), path, "", ""))


def default_backup_path(db_path: Path) -> Path:
    timestamp = time.strftime("%Y%m%d-%H%M%S")
    return db_path.with_name(f"{db_path.name}.before-github-asset-localize-{timestamp}")


def backup_database(conn: sqlite3.Connection, backup_path: Path) -> None:
    backup_path = backup_path.resolve()
    if backup_path.exists():
        raise FileExistsError(f"backup path already exists: {backup_path}")
    backup_path.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(backup_path) as backup:
        conn.backup(backup)


def storage_owner(path: Path) -> tuple[int, int] | None:
    with contextlib.suppress(FileNotFoundError):
        stat = path.stat()
        return stat.st_uid, stat.st_gid
    with contextlib.suppress(FileNotFoundError):
        stat = path.parent.stat()
        return stat.st_uid, stat.st_gid
    return None


def localize_assets(
    conn: sqlite3.Connection,
    assets: collections.OrderedDict[tuple[int, str], Asset],
    attachments_dir: Path,
    local_url_base: str,
    owner: tuple[int, int] | None,
    args: argparse.Namespace,
) -> collections.Counter[str]:
    stats: collections.Counter[str] = collections.Counter()
    tmp_dir = args.tmp_dir.resolve() if args.tmp_dir else attachments_dir / "tmp"
    tmp_dir.mkdir(parents=True, exist_ok=True)
    if owner is not None:
        chown_path(tmp_dir, owner)

    processed = 0
    for asset in assets.values():
        if args.limit_assets is not None and processed >= args.limit_assets:
            asset.skipped = "asset limit reached"
            stats["skipped"] += 1
            continue
        processed += 1

        try:
            downloaded = download_asset(asset.fetch_url, args, tmp_dir)
            if not is_image(downloaded, asset.fetch_url):
                asset.skipped = f"not an image ({downloaded.content_type})"
                stats["skipped"] += 1
                downloaded.path.unlink(missing_ok=True)
                continue

            name = attachment_name(asset.canonical_url, downloaded.final_url, downloaded.content_type)
            existing = find_existing_attachment(conn, asset.context.repo_id, name, attachments_dir)
            if existing and existing["file_exists"]:
                downloaded.path.unlink(missing_ok=True)
                asset.local_url = local_url_base + urllib.parse.quote(existing["uuid"])
                stats["reused"] += 1
                continue

            if existing:
                final_path = attachment_path(attachments_dir, existing["uuid"])
                install_download(downloaded.path, final_path, owner)
                update_attachment_size(conn, existing["id"], downloaded.size)
                asset.local_url = local_url_base + urllib.parse.quote(existing["uuid"])
                stats["refetched"] += 1
                continue

            attach_uuid = str(uuid.uuid4())
            final_path = attachment_path(attachments_dir, attach_uuid)
            install_download(downloaded.path, final_path, owner)
            try:
                insert_attachment(conn, attach_uuid, name, downloaded.size, asset.context)
            except Exception:
                final_path.unlink(missing_ok=True)
                raise
            asset.local_url = local_url_base + urllib.parse.quote(attach_uuid)
            stats["created"] += 1
        except Exception as exc:
            asset.error = str(exc)
            stats["failed"] += 1
    return stats


def download_asset(url: str, args: argparse.Namespace, tmp_dir: Path) -> DownloadedAsset:
    request = urllib.request.Request(url, headers={"User-Agent": args.user_agent})
    tmp = tempfile.NamedTemporaryFile(prefix="github-asset-", dir=tmp_dir, delete=False)
    tmp_path = Path(tmp.name)
    size = 0
    content_type = ""
    final_url = url
    try:
        with tmp:
            with urllib.request.urlopen(request, timeout=args.http_timeout) as response:
                status = getattr(response, "status", 200)
                if status < 200 or status >= 300:
                    raise RuntimeError(f"HTTP {status}")
                content_type = response.headers.get_content_type().lower()
                final_url = response.geturl()
                while True:
                    chunk = response.read(1024 * 256)
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > args.max_bytes:
                        raise RuntimeError(f"image exceeds {args.max_bytes} byte limit")
                    tmp.write(chunk)
    except (urllib.error.URLError, OSError, RuntimeError):
        tmp_path.unlink(missing_ok=True)
        raise
    return DownloadedAsset(path=tmp_path, size=size, content_type=content_type, final_url=final_url)


def is_image(downloaded: DownloadedAsset, source_url: str) -> bool:
    if downloaded.content_type.startswith("image/"):
        return True
    return extension_from_url(source_url) in IMAGE_EXTENSIONS


def attachment_name(canonical_url: str, final_url: str, content_type: str) -> str:
    parsed = urllib.parse.urlsplit(canonical_url)
    host = parsed.netloc.lower()
    path_parts = [part for part in urllib.parse.unquote(parsed.path).split("/") if part]

    if host == "user-images.githubusercontent.com":
        parts = ["user-images", *path_parts]
    elif host == "private-user-images.githubusercontent.com":
        parts = ["private-user-images", *path_parts]
    elif host == "github.com":
        if "user-attachments" in path_parts:
            idx = path_parts.index("user-attachments")
            parts = path_parts[idx:]
        else:
            parts = path_parts
    elif host.startswith("github-production-user-asset-") and host.endswith(".s3.amazonaws.com"):
        parts = path_parts
    else:
        parts = [host, *path_parts]

    ext = extension_from_url(canonical_url) or extension_from_url(final_url) or extension_from_content_type(content_type)
    if parts:
        last_ext = Path(parts[-1]).suffix.lower()
        if last_ext in IMAGE_EXTENSIONS:
            parts[-1] = parts[-1][: -len(last_ext)]
    stem = slugify("-".join(parts)) or "asset"
    digest = hashlib.sha256(canonical_url.encode()).hexdigest()[:12]
    stem = f"github-asset-{stem}"
    max_stem_len = 180 - len(ext)
    if len(stem) > max_stem_len:
        stem = f"{stem[: max_stem_len - len(digest) - 1].rstrip('-')}-{digest}"
    return stem + ext


def extension_from_url(url: str) -> str:
    parsed = urllib.parse.urlsplit(url)
    ext = Path(urllib.parse.unquote(parsed.path)).suffix.lower()
    return ext if ext in IMAGE_EXTENSIONS else ""


def extension_from_content_type(content_type: str) -> str:
    if content_type in EXTENSION_BY_CONTENT_TYPE:
        return EXTENSION_BY_CONTENT_TYPE[content_type]
    ext = mimetypes.guess_extension(content_type) or ".img"
    if ext == ".jpe":
        ext = ".jpg"
    return ext


def slugify(value: str) -> str:
    value = value.lower()
    value = re.sub(r"[^a-z0-9._-]+", "-", value)
    value = re.sub(r"-{2,}", "-", value)
    return value.strip(".-_")


def find_existing_attachment(
    conn: sqlite3.Connection,
    repo_id: int,
    name: str,
    attachments_dir: Path,
) -> sqlite3.Row | None:
    rows = conn.execute(
        """
        SELECT id, uuid, size
        FROM attachment
        WHERE repo_id = ? AND name = ? AND COALESCE(external_url, '') = ''
        ORDER BY id ASC
        """,
        (repo_id, name),
    ).fetchall()
    for row in rows:
        if attachment_path(attachments_dir, row["uuid"]).is_file():
            return row_with_file_exists(row, True)
    if rows:
        return row_with_file_exists(rows[0], False)
    return None


def row_with_file_exists(row: sqlite3.Row, exists: bool) -> dict[str, object]:
    return {**dict(row), "file_exists": exists}


def attachment_path(attachments_dir: Path, attach_uuid: str) -> Path:
    return attachments_dir / attach_uuid[0] / attach_uuid[1] / attach_uuid


def install_download(tmp_path: Path, final_path: Path, owner: tuple[int, int] | None) -> None:
    final_path.parent.mkdir(parents=True, exist_ok=True)
    if owner is not None:
        chown_path(final_path.parent.parent, owner)
        chown_path(final_path.parent, owner)
    os.replace(tmp_path, final_path)
    os.chmod(final_path, 0o666 & ~current_umask())
    if owner is not None:
        chown_path(final_path, owner)


def current_umask() -> int:
    mask = os.umask(0)
    os.umask(mask)
    return mask


def chown_path(path: Path, owner: tuple[int, int]) -> None:
    with contextlib.suppress(PermissionError, FileNotFoundError):
        os.chown(path, owner[0], owner[1])


def update_attachment_size(conn: sqlite3.Connection, attachment_id: int, size: int) -> None:
    with conn:
        conn.execute("UPDATE attachment SET size = ? WHERE id = ?", (size, attachment_id))


def insert_attachment(conn: sqlite3.Connection, attach_uuid: str, name: str, size: int, context: Context) -> None:
    created_unix = context.created_unix or int(time.time())
    with conn:
        conn.execute(
            """
            INSERT INTO attachment (
                uuid, uploader_id, repo_id, issue_id, release_id, comment_id,
                name, download_count, size, created_unix, external_url
            ) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, '')
            """,
            (
                attach_uuid,
                context.uploader_id,
                context.repo_id,
                context.issue_id,
                context.release_id,
                context.comment_id,
                name,
                size,
                created_unix,
            ),
        )


def rewrite_rows(
    conn: sqlite3.Connection,
    rows: list[ContentRow],
    assets: collections.OrderedDict[tuple[int, str], Asset],
) -> collections.Counter[str]:
    by_exact_url: dict[tuple[int, str], str] = {}
    for asset in assets.values():
        if asset.local_url is None:
            continue
        for exact_url in asset.exact_urls:
            by_exact_url[(asset.repo_id, exact_url)] = asset.local_url

    stats: collections.Counter[str] = collections.Counter()
    for row in rows:
        content = row.content
        rewritten = content
        for exact_url in row_exact_urls(content):
            local_url = by_exact_url.get((row.context.repo_id, exact_url))
            if local_url:
                rewritten = rewritten.replace(exact_url, local_url)
        if rewritten == content:
            stats["unchanged"] += 1
            continue
        with conn:
            cur = conn.execute(
                f"UPDATE {row.table} SET {row.column} = ? WHERE id = ? AND {row.column} = ?",
                (rewritten, row.row_id, content),
            )
        if cur.rowcount == 1:
            stats["updated"] += 1
        else:
            stats["conflicted"] += 1
    return stats


if __name__ == "__main__":
    raise SystemExit(main())
