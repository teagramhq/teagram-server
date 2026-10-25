#!/usr/bin/env python3
"""Build the pinned free emoji art sources into a static sticker bundle."""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import json
import shutil
import struct
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable


MAX_STICKERS_PER_PACK = 120
MAX_BUNDLE_BYTES = 150_000_000
MAX_WORKERS = 12
GROUPS = (
    ("Smileys & Emotion", "smileys_emotion"),
    ("People & Body", "people_body"),
    ("Animals & Nature", "animals_nature"),
    ("Food & Drink", "food_drink"),
)
SKIN_TONE_MODIFIERS = set(range(0x1F3FB, 0x1F400))
VARIATION_SELECTORS = {0xFE0E, 0xFE0F}


class GeneratorError(Exception):
    """An input or conversion error suitable for the command line."""


@dataclass(frozen=True)
class Emoji:
    codepoints: tuple[int, ...]
    text: str
    group: str = ""


@dataclass(frozen=True)
class Sticker:
    emoji: Emoji
    file: str


@dataclass(frozen=True)
class Pack:
    short_name: str
    source: str
    license_id: str
    attribution: str
    stickers: tuple[Sticker, ...]


def normalized_codepoints(codepoints: Iterable[int]) -> tuple[int, ...]:
    return tuple(cp for cp in codepoints if cp not in VARIATION_SELECTORS)


def emoji_filename(codepoints: Iterable[int]) -> str:
    return "-".join(f"{cp:x}" for cp in codepoints)


def parse_emoji_test(contents: str, selected_groups: set[str]) -> list[Emoji]:
    result: list[Emoji] = []
    current_group = ""
    seen: set[tuple[int, ...]] = set()
    for line in contents.splitlines():
        if line.startswith("# group: "):
            current_group = line.removeprefix("# group: ").strip()
            continue
        if not line or line.startswith("#") or current_group not in selected_groups:
            continue
        fields = line.split("#", 1)[0].split(";", 1)
        if len(fields) != 2 or fields[1].strip() != "fully-qualified":
            continue
        codepoints = tuple(int(part, 16) for part in fields[0].split())
        if not codepoints or any(cp in SKIN_TONE_MODIFIERS for cp in codepoints):
            continue
        key = normalized_codepoints(codepoints)
        if key in seen:
            continue
        seen.add(key)
        result.append(Emoji(codepoints, "".join(chr(cp) for cp in codepoints), current_group))
    if not result:
        raise GeneratorError("Unicode emoji data contains no selected fully-qualified emoji")
    return result


def make_packs(
    source_id: str,
    source_name: str,
    license_id: str,
    group_name: str,
    emojis: Iterable[Emoji],
    available: set[tuple[int, ...]],
    attribution: str = "",
) -> list[Pack]:
    slug = dict(GROUPS).get(group_name)
    if slug is None:
        raise GeneratorError(f"unknown emoji group {group_name!r}")
    stickers = [
        Sticker(emoji, "")
        for emoji in emojis
        if normalized_codepoints(emoji.codepoints) in available
    ]
    if not stickers:
        return []
    numbered = len(stickers) > MAX_STICKERS_PER_PACK
    packs: list[Pack] = []
    for start in range(0, len(stickers), MAX_STICKERS_PER_PACK):
        chunk = stickers[start : start + MAX_STICKERS_PER_PACK]
        sequence = start // MAX_STICKERS_PER_PACK + 1
        suffix = f"_{sequence:02d}" if numbered else ""
        short_name = f"{source_id}_{slug}{suffix}"
        named_stickers = tuple(
            Sticker(sticker.emoji, f"{short_name}/{emoji_filename(sticker.emoji.codepoints)}.webp")
            for sticker in chunk
        )
        name_suffix = f" {sequence:02d}" if numbered else ""
        packs.append(
            Pack(
                short_name=short_name,
                source=f"{source_name} {group_name}{name_suffix}",
                license_id=license_id,
                attribution=attribution,
                stickers=named_stickers,
            )
        )
    return packs


def source_digest(downloaded_files: dict[str, bytes]) -> str:
    digest = hashlib.sha256()
    for path, content in sorted(downloaded_files.items()):
        encoded_path = path.encode("utf-8")
        digest.update(struct.pack(">I", len(encoded_path)))
        digest.update(encoded_path)
        digest.update(struct.pack(">Q", len(content)))
        digest.update(content)
    return digest.hexdigest()


def verify_source_checksum(source_name: str, expected: str, downloaded_files: dict[str, bytes]) -> str:
    actual = source_digest(downloaded_files)
    if not expected or actual != expected:
        raise GeneratorError(
            f"{source_name} source download checksum mismatch: "
            f"expected {expected or 'a pinned SHA-256'}, got {actual}"
        )
    return actual


def git_blob_sha(content: bytes) -> str:
    header = f"blob {len(content)}\0".encode("ascii")
    return hashlib.sha1(header + content).hexdigest()


def request_bytes(url: str, context: str, retries: int = 3) -> bytes:
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "teagram-sticker-bundle-generator",
        },
    )
    last_error: Exception | None = None
    for attempt in range(retries):
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return response.read()
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            last_error = error
            if attempt + 1 < retries:
                time.sleep(attempt + 1)
    raise GeneratorError(f"{context}: download failed: {last_error}")


def request_json(url: str, context: str) -> dict:
    try:
        return json.loads(request_bytes(url, context).decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise GeneratorError(f"{context}: invalid JSON response: {error}") from error


def resolve_tag(source: dict) -> None:
    tag = source.get("tag")
    if not tag:
        return
    name = source["name"]
    repo = source["repo"]
    escaped_tag = urllib.parse.quote(tag, safe="/")
    url = f"https://api.github.com/repos/{repo}/git/ref/tags/{escaped_tag}"
    try:
        target = request_json(url, f"{name} source tag {tag}")["object"]
        for _ in range(5):
            if target.get("type") != "tag":
                break
            target = request_json(
                f"https://api.github.com/repos/{repo}/git/tags/{target['sha']}",
                f"{name} source tag {tag}",
            )["object"]
        else:
            raise GeneratorError(f"{name} source tag {tag} has too many nested tag objects")
    except (KeyError, TypeError) as error:
        raise GeneratorError(f"{name} source tag {tag} could not be resolved") from error
    if target.get("type") != "commit" or target.get("sha") != source["commit"]:
        actual = target.get("sha", "unknown")
        raise GeneratorError(
            f"{name} source tag {tag} moved: resolved to {actual}, expected {source['commit']}"
        )


def source_tree(source: dict) -> dict[str, dict]:
    name = source["name"]
    url = (
        f"https://api.github.com/repos/{source['repo']}/git/trees/"
        f"{source['commit']}?recursive=1"
    )
    tree_response = request_json(url, f"{name} source tree")
    if tree_response.get("truncated"):
        raise GeneratorError(f"{name} source tree response was truncated")
    entries = tree_response.get("tree")
    if not isinstance(entries, list):
        raise GeneratorError(f"{name} source tree response is missing its file list")
    return {entry["path"]: entry for entry in entries if isinstance(entry, dict) and "path" in entry}


def download_source_files(source: dict, entries: dict[str, dict]) -> dict[str, bytes]:
    name = source["name"]
    for path, entry in entries.items():
        if entry.get("type") != "blob" or entry.get("mode") not in {"100644", "100755"}:
            raise GeneratorError(f"{name} source file {path} is not a regular file")

    def fetch(item: tuple[str, dict]) -> tuple[str, bytes]:
        path, entry = item
        encoded_path = urllib.parse.quote(path, safe="/")
        url = f"https://raw.githubusercontent.com/{source['repo']}/{source['commit']}/{encoded_path}"
        content = request_bytes(url, f"{name} source file {path}")
        if git_blob_sha(content) != entry["sha"]:
            raise GeneratorError(f"{name} source file {path} failed its Git object checksum")
        return path, content

    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as pool:
        return dict(pool.map(fetch, sorted(entries.items())))


def source_asset_path(source: dict, codepoints: tuple[int, ...], tree: dict[str, dict]) -> str | None:
    for key in (codepoints, normalized_codepoints(codepoints)):
        if source["kind"] == "twemoji":
            filename = emoji_filename(key).lower()
            path = f"assets/svg/{filename}.svg"
        elif source["kind"] == "noto":
            filename = "_".join(f"{cp:x}" for cp in key)
            path = f"2D/svg/emoji_u{filename}.svg"
        elif source["kind"] == "openmoji":
            filename = emoji_filename(key).upper()
            path = f"color/svg/{filename}.svg"
        else:
            raise GeneratorError(f"{source['name']} source has an unknown asset layout")
        if path in tree:
            return path
    return None


def parse_fluent_metadata(source: dict, files: dict[str, bytes], tree: dict[str, dict]) -> dict[tuple[int, ...], str]:
    name = source["name"]
    colors_by_parent: dict[str, list[str]] = {}
    for path in tree:
        if path.startswith("assets/") and "/Color/" in path and path.endswith(".svg"):
            parent = path.rsplit("/Color/", 1)[0]
            colors_by_parent.setdefault(parent, []).append(path)
    index: dict[tuple[int, ...], str] = {}
    for path, contents in files.items():
        if not path.endswith("/metadata.json"):
            continue
        try:
            metadata = json.loads(contents.decode("utf-8"))
            codepoints = tuple(int(value, 16) for value in metadata["unicode"].split())
        except (UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError, ValueError) as error:
            raise GeneratorError(f"{name} source metadata {path} is invalid: {error}") from error
        key = normalized_codepoints(codepoints)
        color_paths = colors_by_parent.get(path.rsplit("/", 1)[0], [])
        if not key or len(color_paths) != 1:
            continue
        existing = index.get(key)
        if existing is not None and existing != color_paths[0]:
            raise GeneratorError(f"{name} source has duplicate Color art for {emoji_filename(key)}")
        index[key] = color_paths[0]
    return index


def selected_source_files(
    source: dict,
    tree: dict[str, dict],
    emojis: list[Emoji],
) -> tuple[dict[str, dict], dict[tuple[int, ...], str], dict[str, bytes]]:
    name = source["name"]
    license_path = source["license_file"]
    if license_path not in tree:
        raise GeneratorError(f"{name} source revision is missing license file {license_path}")
    selected = {license_path: tree[license_path]}
    assets: dict[tuple[int, ...], str] = {}

    prefetched: dict[str, bytes] = {}
    if source["kind"] == "fluent":
        metadata_entries = {
            path: entry
            for path, entry in tree.items()
            if path.startswith("assets/") and path.endswith("/metadata.json")
        }
        metadata_files = download_source_files(source, metadata_entries)
        prefetched.update(metadata_files)
        for path, entry in metadata_entries.items():
            selected[path] = entry
        index = parse_fluent_metadata(source, metadata_files, tree)
        for emoji in emojis:
            key = normalized_codepoints(emoji.codepoints)
            if key in index:
                assets[key] = index[key]
    else:
        for emoji in emojis:
            key = normalized_codepoints(emoji.codepoints)
            path = source_asset_path(source, emoji.codepoints, tree)
            if path:
                assets[key] = path

    for path in assets.values():
        if path not in tree:
            raise GeneratorError(f"{name} source metadata names missing artwork file {path}")
        selected[path] = tree[path]
    return selected, assets, prefetched


def load_source_inputs(source: dict, emojis: list[Emoji], print_digests: bool) -> tuple[dict[str, bytes], dict[tuple[int, ...], str]]:
    resolve_tag(source)
    tree = source_tree(source)
    selected, assets, prefetched = selected_source_files(source, tree, emojis)
    pending = {path: entry for path, entry in selected.items() if path not in prefetched}
    downloaded = {**prefetched, **download_source_files(source, pending)}

    license_file = source["license_file"]
    license_text = downloaded[license_file].decode("utf-8", errors="replace")
    if source["license_marker"] not in license_text:
        raise GeneratorError(
            f"{source['name']} source license {license_file} does not confirm {source['license']}"
        )

    expected_digest = source.get("download_sha256", "")
    if not print_digests:
        verify_source_checksum(source["name"], expected_digest, downloaded)
    return downloaded, assets


class RendererWorker:
    def __enter__(self) -> RendererWorker:
        script = Path(__file__).with_name("render.mjs")
        try:
            self.process = subprocess.Popen(
                ["node", str(script)],
                cwd=script.parent,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                bufsize=1,
            )
        except FileNotFoundError as error:
            raise GeneratorError("Node.js is required; run tools/stickerbundle/generate.sh") from error
        return self

    def render(self, svg: bytes, context: str) -> bytes:
        assert self.process.stdin is not None and self.process.stdout is not None
        request = json.dumps({"svg": base64.b64encode(svg).decode("ascii")})
        try:
            self.process.stdin.write(request + "\n")
            self.process.stdin.flush()
            response_line = self.process.stdout.readline()
            response = json.loads(response_line)
        except (BrokenPipeError, json.JSONDecodeError, OSError) as error:
            raise GeneratorError(f"{context}: image renderer failed: {error}") from error
        if "error" in response:
            raise GeneratorError(f"{context}: image renderer failed: {response['error']}")
        try:
            return base64.b64decode(response["webp"], validate=True)
        except (KeyError, ValueError) as error:
            raise GeneratorError(f"{context}: image renderer returned an invalid response") from error

    def __exit__(self, error_type, error, traceback) -> None:
        if self.process.stdin is not None:
            self.process.stdin.close()
        return_code = self.process.wait()
        if self.process.stderr is not None:
            detail = self.process.stderr.read().strip()
        else:
            detail = ""
        if error_type is None and return_code != 0:
            raise GeneratorError(f"image renderer exited with status {return_code}: {detail}")


def render_webp(svg: bytes, renderer: RendererWorker, context: str) -> tuple[bytes, int, str]:
    content = renderer.render(svg, context)
    if len(content) < 12 or content[:4] != b"RIFF" or content[8:12] != b"WEBP":
        raise GeneratorError(f"{context}: converter did not produce a WebP file")
    return content, len(content), hashlib.sha256(content).hexdigest()


def manifest_bytes(packs: list[dict]) -> bytes:
    value = {"version": 1, "packs": packs}
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def source_manifest(
    pack: Pack,
    source: dict,
    inputs: dict[str, bytes],
    assets: dict[tuple[int, ...], str],
    stage: Path,
    renderer: RendererWorker,
) -> dict:
    file_lookup = {path: content for path, content in inputs.items()}
    manifest_stickers = []
    for sticker in pack.stickers:
        key = normalized_codepoints(sticker.emoji.codepoints)
        asset_path = assets[key]
        context = f"{source['name']} {sticker.emoji.text}"
        output_path = stage / sticker.file
        content, size, checksum = render_webp(file_lookup[asset_path], renderer, context)
        output_path.parent.mkdir(parents=True, exist_ok=True)
        output_path.write_bytes(content)
        manifest_stickers.append(
            {
                "emoji": sticker.emoji.text,
                "file": sticker.file,
                "size": size,
                "sha256": checksum,
            }
        )
    return {
        "short_name": pack.short_name,
        "source": pack.source,
        "license": pack.license_id,
        "attribution": pack.attribution,
        "stickers": manifest_stickers,
    }


def load_config() -> dict:
    path = Path(__file__).with_name("sources.json")
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise GeneratorError(f"read {path}: {error}") from error


def write_bundle(output: Path, configuration: dict, print_digests: bool) -> tuple[int, int, int]:
    if not print_digests and output.exists():
        raise GeneratorError(f"output path already exists: {output}")
    emoji_data = configuration["unicode_emoji"]
    unicode_content = request_bytes(emoji_data["url"], f"{emoji_data['name']} source")
    unicode_sha = hashlib.sha256(unicode_content).hexdigest()
    if unicode_sha != emoji_data["sha256"]:
        raise GeneratorError(
            f"{emoji_data['name']} source checksum mismatch: expected {emoji_data['sha256']}, got {unicode_sha}"
        )
    group_names = {name for name, _ in GROUPS}
    parsed_emojis = parse_emoji_test(unicode_content.decode("utf-8"), group_names)
    grouped = {name: [emoji for emoji in parsed_emojis if emoji.group == name] for name, _ in GROUPS}

    source_inputs: dict[str, tuple[dict[str, bytes], dict[tuple[int, ...], str]]] = {}
    for source in configuration["sources"]:
        downloaded, assets = load_source_inputs(source, parsed_emojis, print_digests)
        source_inputs[source["id"]] = (downloaded, assets)
        if print_digests:
            print(f"{source['name']}: {source_digest(downloaded)} ({len(downloaded)} files)")
    if print_digests:
        return 0, 0, 0

    output.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{output.name}.", dir=output.parent))
    try:
        with RendererWorker() as renderer:
            all_packs: list[dict] = []
            for source in configuration["sources"]:
                downloaded, assets = source_inputs[source["id"]]
                available = set(assets)
                for group_name, _ in GROUPS:
                    packs = make_packs(
                        source_id=source["id"],
                        source_name=source["name"],
                        license_id=source["license"],
                        group_name=group_name,
                        emojis=grouped[group_name],
                        available=available,
                        attribution=source["attribution"],
                    )
                    for pack in packs:
                        all_packs.append(source_manifest(pack, source, downloaded, assets, stage, renderer))
        if not all_packs:
            raise GeneratorError("no sticker packs were produced")
        bundle_manifest = manifest_bytes(all_packs)
        (stage / "manifest.json").write_bytes(bundle_manifest)
        file_size = sum(path.stat().st_size for path in stage.rglob("*") if path.is_file())
        if file_size > MAX_BUNDLE_BYTES:
            raise GeneratorError(
                f"bundle size is {file_size} bytes, above the {MAX_BUNDLE_BYTES}-byte limit"
            )
        stage.chmod(0o755)
        stage.rename(output)
        sticker_count = sum(len(pack["stickers"]) for pack in all_packs)
        return len(all_packs), sticker_count, file_size
    except Exception:
        shutil.rmtree(stage)
        raise


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="new directory for the generated bundle")
    parser.add_argument(
        "--print-source-digests",
        action="store_true",
        help="download pinned source files and print their aggregate SHA-256 values",
    )
    args = parser.parse_args(argv)
    if not args.output and not args.print_source_digests:
        parser.error("--output is required unless --print-source-digests is used")
    started = time.monotonic()
    try:
        packs, stickers, size = write_bundle(args.output or Path("."), load_config(), args.print_source_digests)
        if not args.print_source_digests:
            elapsed = time.monotonic() - started
            print(f"Generated {packs} packs with {stickers} stickers ({size} bytes) in {elapsed:.1f}s")
        return 0
    except (GeneratorError, OSError, KeyError, ValueError) as error:
        print(error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
