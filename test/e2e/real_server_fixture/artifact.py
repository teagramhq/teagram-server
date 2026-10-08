#!/usr/bin/env python3
"""Stage and independently audit a caller-built private web artifact."""

from __future__ import annotations

import argparse
import base64
import hashlib
import html.parser
import ipaddress
import json
import os
import re
import shutil
import stat
import subprocess
import sys
from pathlib import Path


MAX_ARTIFACT_FILES = 5000
MAX_ARTIFACT_ENTRIES = 10000
MAX_ARTIFACT_BYTES = 128 * 1024 * 1024
MAX_ARTIFACT_DEPTH = 64
MAX_PRODUCT_REFERENCES = 200
ENDPOINT = "wss://telegramd.test/apiws"
RESERVED_ROUTES = {"healthz", "_fixture_probe"}
PRODUCTION_IDENTIFIERS = (b"telegram-server.tailaa4918.ts.net", b"fbb62871f07fae2a")
PRIVATE_KEY_BLOCK = re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----", re.IGNORECASE)

# Harness-owned transport signatures. They are pinned here on purpose: the
# audit never reads them from the web tree or the artifact under test.
OFFICIAL_MT_PROTO_ROUTE = re.compile(
    rb"(?:kws[1-5](?:-1)?|pluto(?:-1)?|venus(?:-1)?|aurora(?:-1)?|vesta(?:-1)?|flora(?:-1)?)"
    rb"\.web\.telegram\.org(?:[/:?#\"'`\s]|$)|web\.telegram\.org/(?:apiw(?:s|_test1|1)?)(?:[/:?#\"'`\s]|$)",
    re.IGNORECASE,
)
OFFICIAL_MT_PROTO_DYNAMIC_ROUTE = re.compile(
    rb"\$\{[^}]+\}[^`]*\.web\.telegram\.org|"
    rb"['\"`]\.?web\.telegram\.org/?['\"`]\s*\+|\+\s*['\"`]\.?web\.telegram\.org/?['\"`]|"
    rb"['\"`]wss?://[^'\"`]*['\"`]\s*\+",
    re.IGNORECASE,
)
OFFICIAL_DC_HOST = re.compile(rb"(?:[a-z0-9-]+\.)+web\.telegram\.org(?:[/:?#\"'`\s]|$)", re.IGNORECASE)
MTPROTO_ROUTE_PATH = re.compile(rb"https?://[^\s\"'<>`]+/apiw(?:_test1|1)?(?:[/?#\"'`\s]|$)", re.IGNORECASE)
CLEARTEXT_WEBSOCKET = re.compile(rb"\bws://", re.IGNORECASE)
OFFICIAL_DC_IPV6_PREFIX = re.compile(rb"2001:0*b28:f23|2001:0*67c:4e8", re.IGNORECASE)
WSS_SCHEME = b"wss://"
PRODUCT_REFERENCE = re.compile(
    rb"https?://(?:(?:[a-z0-9-]+\.)+)?(?:telegram\.org|t\.me|telegram\.me|telesco\.pe)(?::\d+)?"
    rb"[^\s\"'<>`\\,;)}\]]*",
    re.IGNORECASE,
)
IPV4 = re.compile(rb"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")
IPV6 = re.compile(rb"(?<![0-9a-f:])(?:[0-9a-f]{0,4}:){2,}[0-9a-f:.]{0,39}(?![0-9a-f:])", re.IGNORECASE)
TELEGRAM_DC_NETWORKS = tuple(
    ipaddress.ip_network(value)
    for value in (
        "149.154.160.0/20",
        "91.108.4.0/22",
        "91.108.8.0/21",
        "91.108.12.0/22",
        "91.108.16.0/22",
        "91.108.56.0/22",
        "95.161.64.0/20",
        "2001:67c:4e8::/48",
        "2001:b28:f23d::/48",
        "2001:b28:f23f::/48",
        "2001:b28:f242::/48",
    )
)

# Trusted Telegram MTProto RSA fingerprints and moduli, pinned by the harness.
# A bundle carrying one of these can complete MTProto against a real Telegram
# datacenter, so it is transport material, not an ordinary product reference.
TRUSTED_MT_PROTO_FINGERPRINTS = (
    "c3b42b026ce86b21",
    "0bc35f3509f7b7a5",
    "15ae5fa8b5529542",
    "aeae98e13cd7f94f",
    "5a181b2235057d98",
    "b25898df208d2603",
    "d09d1d85de64fd85",
)
TRUSTED_MT_PROTO_MODULI = (
    "c150023e2f70db7985ded064759cfecf0af328e69a41daf4d6f01b538135a6f91f8f8b2a0ec9ba9720ce352efcf6c5680ffc424bd634864902de0b4bd6d49f4e580230e3ae97d95c8b19442b3c0a10d8f5633fecedd6926a7f6dab0ddb7d457f9ea81b8465fcd6fffeed114011df91c059caedaf97625f6c96ecc74725556934ef781d866b34f011fce4d835a090196e9a5f0e4449af7eb697ddb9076494ca5f81104a305b6dd27665722c46b60e5df680fb16b210607ef217652e60236c255f6a28315f4083a96791d7214bf64c1df4fd0db1944fb26a2a57031b32eee64ad15a8ba68885cde74a5bfc920f6abf59ba5c75506373e7130f9042da922179251f",
    "aeec36c8ffc109cb099624685b97815415657bd76d8c9c3e398103d7ad16c9bba6f525ed0412d7ae2c2de2b44e77d72cbf4b7438709a4e646a05c43427c7f184debf72947519680e651500890c6832796dd11f772c25ff8f576755afe055b0a3752c696eb7d8da0d8be1faf38c9bdd97ce0a77d3916230c4032167100edd0f9e7a3a9b602d04367b689536af0d64b613ccba7962939d3b57682beb6dae5b608130b2e52aca78ba023cf6ce806b1dc49c72cf928a7199d22e3d7ac84e47bc9427d0236945d10dbd15177bab413fbf0edfda09f014c7a7da088dde9759702ca760af2b8e4e97cc055c617bd74c3d97008635b98dc4d621b4891da9fb0473047927",
    "bdf2c77d81f6afd47bd30f29ac76e55adfe70e487e5e48297e5a9055c9c07d2b93b4ed3994d3eca5098bf18d978d54f8b7c713eb10247607e69af9ef44f38e28f8b439f257a11572945cc0406fe3f37bb92b79112db69eedf2dc71584a661638ea5becb9e23585074b80d57d9f5710dd30d2da940e0ada2f1b878397dc1a72b5ce2531b6f7dd158e09c828d03450ca0ff8a174deacebcaa22dde84ef66ad370f259d18af806638012da0ca4a70baa83d9c158f3552bc9158e69bf332a45809e1c36905a5caa12348dd57941a482131be7b2355a5f4635374f3bd3ddf5ff925bf4809ee27c1e67d9120c5fe08a9de458b1b4a3c5d0a428437f2beca81f4e2d5ff",
    "b3f762b739be98f343eb1921cf0148cfa27ff7af02b6471213fed9daa0098976e667750324f1abcea4c31e43b7d11f1579133f2b3d9fe27474e462058884e5e1b123be9cbbc6a443b2925c08520e7325e6f1a6d50e117eb61ea49d2534c8bb4d2ae4153fabe832b9edf4c5755fdd8b19940b81d1d96cf433d19e6a22968a85dc80f0312f596bd2530c1cfb28b5fe019ac9bc25cd9c2a5d8a0f3a1c0c79bcca524d315b5e21b5c26b46babe3d75d06d1cd33329ec782a0f22891ed1db42a1d6c0dea431428bc4d7aabdcf3e0eb6fda4e23eb7733e7727e9a1915580796c55188d2596d2665ad1182ba7abf15aaa5a8b779ea996317a20ae044b820bff35b6e8a1",
    "be6a71558ee577ff03023cfa17aab4e6c86383cff8a7ad38edb9fafe6f323f2d5106cbc8cafb83b869cffd1ccf121cd743d509e589e68765c96601e813dc5b9dfc4be415c7a6526132d0035ca33d6d6075d4f535122a1cdfe017041f1088d1419f65c8e5490ee613e16dbf662698c0f54870f0475fa893fc41eb55b08ff1ac211bc045ded31be27d12c96d8d3cfc6a7ae8aa50bf2ee0f30ed507cc2581e3dec56de94f5dc0a7abee0be990b893f2887bd2c6310a1e0a9e3e38bd34fded2541508dc102a9c9b4c95effd9dd2dfe96c29be647d6c69d66ca500843cfaed6e440196f1dbe0e2e22163c61ca48c79116fa77216726749a976a1c4b0944b5121e8c01",
    "c8c11d635691fac091dd9489aedced2932aa8a0bcefef05fa800892d9b52ed03200865c9e97211cb2ee6c7ae96d3fb0e15aeffd66019b44a08a240cfdd2868a85e1f54d6fa5deaa041f6941ddf302690d61dc476385c2fa655142353cb4e4b59f6e5b6584db76fe8b1370263246c010c93d011014113ebdf987d093f9d37c2be48352d69a1683f8f6e6c2167983c761e3ab169fde5daaa12123fa1beab621e4da5935e9c198f82f35eae583a99386d8110ea6bd1abb0f568759f62694419ea5f69847c43462abef858b4cb5edc84e7b9226cd7bd7e183aa974a712c079dde85b9dc063b8a5c08e8f859c0ee5dcd824c7807f20153361a7f63cfd2a433a1be7f5",
    "e8bb3305c0b52c6cf2afdf7637313489e63e05268e5badb601af417786472e5f93b85438968e20e6729a301c0afc121bf7151f834436f7fda680847a66bf64accec78ee21c0b316f0edafe2f41908da7bd1f4a5107638eeb67040ace472a14f90d9f7c2b7def99688ba3073adb5750bb02964902a359fe745d8170e36876d4fd8a5d41b2a76cbff9a13267eb9580b2d06d10357448d20d9da2191cb5d8c93982961cdfdeda629e37f1fb09a0722027696032fe61ed663db7a37f6f263d370f69db53a0dc0a1748bdaaff6209d5645485e6e001d1953255757e4b8e42813347b11da6ab500fd0ace7e6dfa3736199ccaf9397ed0745a427dcfa6cd67bcb1acff3",
)


def _trusted_rsa_markers() -> tuple[bytes, ...]:
    markers: list[bytes] = []
    for fingerprint in TRUSTED_MT_PROTO_FINGERPRINTS:
        markers.append(fingerprint.encode())
    for modulus in TRUSTED_MT_PROTO_MODULI:
        markers.append(modulus.encode())
        raw = bytes.fromhex(modulus)
        markers.append(base64.b64encode(raw))
        markers.append(base64.urlsafe_b64encode(raw))
    return tuple(markers)


TRUSTED_RSA_MARKERS = _trusted_rsa_markers()
TRUSTED_RSA_MARKER_KEYS = tuple(marker.lower() for marker in TRUSTED_RSA_MARKERS)


class ArtifactError(ValueError):
    """The supplied bundle cannot cross the fixture trust boundary."""


def private_csp(endpoint: str) -> str:
    return "; ".join(
        (
            "default-src 'self'",
            "base-uri 'self'",
            "form-action 'self'",
            "frame-ancestors 'none'",
            "object-src 'none'",
            "script-src 'self' 'wasm-unsafe-eval'",
            "style-src 'self' 'unsafe-inline'",
            "img-src 'self' data: blob:",
            "font-src 'self' data:",
            "media-src 'self' blob:",
            "worker-src 'self' blob:",
            "manifest-src 'self'",
            f"connect-src 'self' {endpoint}",
        )
    ) + ";"


class CSPParser(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.policies: list[str | None] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag.lower() != "meta":
            return
        values = {key.lower(): value for key, value in attrs}
        if (values.get("http-equiv") or "").lower() == "content-security-policy":
            self.policies.append(values.get("content"))

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self.handle_starttag(tag, attrs)


def _resolved(path: str, name: str) -> Path:
    if not path or not os.path.isabs(path):
        raise ArtifactError(f"{name} must be an absolute path")
    try:
        return Path(os.path.realpath(path, strict=True))
    except OSError as error:
        raise ArtifactError(f"{name} is missing or unreadable") from error


def _overlaps(left: Path, right: Path) -> bool:
    return left == right or left in right.parents or right in left.parents


def _open_canonical_directory(path: Path) -> int:
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW
    current = os.open("/", flags)
    try:
        for part in path.parts[1:]:
            next_fd = os.open(part, flags, dir_fd=current)
            os.close(current)
            current = next_fd
        opened = os.fstat(current)
        expected = os.stat(path, follow_symlinks=False)
        if not stat.S_ISDIR(opened.st_mode) or (opened.st_dev, opened.st_ino) != (expected.st_dev, expected.st_ino):
            raise ArtifactError("artifact source changed while it was opened")
        return current
    except Exception:
        os.close(current)
        raise


def _signature(info: os.stat_result) -> tuple[int, int, int, int, int, int]:
    return (info.st_dev, info.st_ino, info.st_mode, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _check_permissions(info: os.stat_result, directory: bool) -> None:
    unsafe = stat.S_ISUID | stat.S_ISGID
    if info.st_mode & unsafe or info.st_mode & 0o022:
        raise ArtifactError("artifact contains unsafe file permissions")
    if directory and info.st_mode & 0o500 != 0o500:
        raise ArtifactError("artifact directory is not readable and searchable by its owner")
    if not directory and not info.st_mode & 0o400:
        raise ArtifactError("artifact file is not readable by its owner")


def _read_regular_file(parent_fd: int, name: str, initial: os.stat_result, remaining_bytes: int) -> bytes:
    if initial.st_nlink != 1:
        raise ArtifactError("artifact contains a hard link")
    _check_permissions(initial, directory=False)
    if initial.st_size < 0 or initial.st_size > remaining_bytes:
        raise ArtifactError("artifact exceeds the total byte limit")
    flags = os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK
    fd = os.open(name, flags, dir_fd=parent_fd)
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or _signature(before) != _signature(initial):
            raise ArtifactError("artifact entry changed while it was opened")
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(fd, min(1024 * 1024, remaining_bytes - total + 1))
            if not chunk:
                break
            total += len(chunk)
            if total > remaining_bytes:
                raise ArtifactError("artifact exceeds the total byte limit")
            chunks.append(chunk)
        after = os.fstat(fd)
        if _signature(after) != _signature(before) or total != before.st_size:
            raise ArtifactError("artifact file changed while it was copied")
        return b"".join(chunks)
    finally:
        os.close(fd)


def _copy_tree(source_fd: int, destination: Path, relative: str, state: dict[str, int]) -> None:
    before = os.fstat(source_fd)
    if not stat.S_ISDIR(before.st_mode):
        raise ArtifactError("artifact contains a non-directory traversal entry")
    _check_permissions(before, directory=True)
    entries = sorted(os.scandir(source_fd), key=lambda entry: entry.name)
    expected_names = [entry.name for entry in entries]
    for entry in entries:
        name = entry.name
        state["entries"] += 1
        if state["entries"] > MAX_ARTIFACT_ENTRIES:
            raise ArtifactError("artifact exceeds the entry count limit")
        if (
            name in {".", ".."}
            or "/" in name
            or "\\" in name
            or "\x00" in name
            or any(ord(character) < 0x20 or ord(character) == 0x7F for character in name)
        ):
            raise ArtifactError("artifact contains an unsafe path")
        try:
            name.encode("utf-8")
        except UnicodeEncodeError as error:
            raise ArtifactError("artifact contains a non-UTF-8 path") from error
        current = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
        if stat.S_ISLNK(current.st_mode):
            raise ArtifactError("artifact contains a symlink")
        child_relative = f"{relative}/{name}" if relative else name
        if stat.S_ISDIR(current.st_mode):
            if child_relative.count("/") + 1 > MAX_ARTIFACT_DEPTH:
                raise ArtifactError("artifact exceeds the directory depth limit")
            child_fd = os.open(
                name,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
                dir_fd=source_fd,
            )
            try:
                opened = os.fstat(child_fd)
                if (opened.st_dev, opened.st_ino) != (current.st_dev, current.st_ino):
                    raise ArtifactError("artifact directory changed while it was opened")
                target = destination / name
                target.mkdir(mode=0o700)
                _copy_tree(child_fd, target, child_relative, state)
                os.chmod(target, 0o555)
            finally:
                os.close(child_fd)
        elif stat.S_ISREG(current.st_mode):
            state["files"] += 1
            if state["files"] > MAX_ARTIFACT_FILES:
                raise ArtifactError("artifact exceeds the file count limit")
            content = _read_regular_file(source_fd, name, current, MAX_ARTIFACT_BYTES - state["bytes"])
            state["bytes"] += len(content)
            target = destination / name
            with target.open("xb") as output:
                output.write(content)
            os.chmod(target, 0o444)
        else:
            raise ArtifactError("artifact contains an unsupported file type")
    after = os.fstat(source_fd)
    actual_names = sorted(entry.name for entry in os.scandir(source_fd))
    if _signature(after) != _signature(before) or actual_names != expected_names:
        raise ArtifactError("artifact directory changed while it was copied")


def _artifact_files(directory: Path) -> list[Path]:
    files: list[Path] = []
    for current, directories, names in os.walk(directory, followlinks=False):
        current_path = Path(current)
        for name in directories:
            info = os.lstat(current_path / name)
            if not stat.S_ISDIR(info.st_mode):
                raise ArtifactError("staged artifact contains a non-directory entry")
        for name in names:
            path = current_path / name
            info = os.lstat(path)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise ArtifactError("staged artifact contains a non-regular file or hard link")
            files.append(path)
    if not files:
        raise ArtifactError("artifact is empty")
    return sorted(files, key=lambda path: path.relative_to(directory).as_posix())


def _digest(files: list[Path], root: Path) -> str:
    digest = hashlib.sha256()
    for path in files:
        relative = path.relative_to(root).as_posix()
        if relative == "mtproto-target.json":
            continue
        content = path.read_bytes()
        digest.update(relative.encode() + b"\0" + str(len(content)).encode() + b"\0")
        digest.update(content + b"\0")
    return "sha256:" + digest.hexdigest()


def _producer_digest(directory: Path) -> tuple[str, str]:
    """Recompute the producer's own digest ordering with the harness-owned script."""
    script = Path(__file__).with_name("artifact_digest.mjs")
    try:
        result = subprocess.run(
            ["node", str(script), str(directory)],
            capture_output=True,
            text=True,
            timeout=180,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ArtifactError("artifact digest could not be recomputed") from error
    if result.returncode != 0:
        raise ArtifactError("artifact digest recomputation failed")
    try:
        report = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise ArtifactError("artifact digest recomputation returned an invalid report") from error
    digest = report.get("digest")
    locale = report.get("collationLocale")
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ArtifactError("artifact digest recomputation returned an invalid digest")
    if not isinstance(locale, str) or not locale:
        raise ArtifactError("artifact digest recomputation returned no collation locale")
    return digest, locale


def _has_alternate_websocket(contents: bytes, endpoint: bytes) -> bool:
    path_continuation = (
        b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
        b"._~!$&()*+=:@%/-"
    )
    path_suffix = endpoint[len(WSS_SCHEME) :]
    position = contents.find(WSS_SCHEME)
    while position != -1:
        if not contents.startswith(path_suffix, position + len(WSS_SCHEME)):
            return True
        following = contents[position + len(endpoint) : position + len(endpoint) + 1]
        if following and following in path_continuation:
            return True
        position = contents.find(WSS_SCHEME, position + len(WSS_SCHEME))
    return False


def _is_dc_address(contents: bytes) -> bool:
    for match in IPV4.finditer(contents):
        try:
            address = ipaddress.ip_address(match.group().decode("ascii"))
        except ValueError:
            continue
        if any(address in network for network in TELEGRAM_DC_NETWORKS if network.version == address.version):
            return True
    for match in IPV6.finditer(contents):
        try:
            address = ipaddress.ip_address(match.group().decode("ascii"))
        except ValueError:
            continue
        if any(address in network for network in TELEGRAM_DC_NETWORKS if network.version == address.version):
            return True
    return False


def _run_secrets(secret_directory: Path) -> list[bytes]:
    directory_info = os.lstat(secret_directory)
    if not stat.S_ISDIR(directory_info.st_mode) or directory_info.st_mode & 0o077:
        raise ArtifactError("fixture secret directory permissions are unsafe")
    values: list[bytes] = []
    for name in ("a-password", "b-password", "authkey.hex", "server-key.pem", "tls.key"):
        path = secret_directory / name
        try:
            info = os.lstat(path)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size == 0 or info.st_mode & 0o077:
                raise ArtifactError("fixture run secret is missing or invalid")
            value = path.read_bytes().strip()
        except OSError as error:
            raise ArtifactError("fixture run secret is missing or unreadable") from error
        values.append(value)
        if name == "authkey.hex":
            try:
                raw_key = bytes.fromhex(value.decode("ascii"))
            except (ValueError, UnicodeDecodeError) as error:
                raise ArtifactError("fixture auth-key secret is invalid") from error
            values.extend((raw_key, value.lower()))
            values.append(base64.b64encode(raw_key))
            values.append(base64.urlsafe_b64encode(raw_key))
        else:
            try:
                decoded = bytes.fromhex(value.decode("ascii"))
            except (ValueError, UnicodeDecodeError):
                continue
            values.extend((decoded, value.lower()))
            values.append(base64.b64encode(decoded))
            values.append(base64.urlsafe_b64encode(decoded))
    return values


def _check_routes(files: list[Path], root: Path) -> bool:
    for path in files:
        relative = path.relative_to(root).as_posix()
        first = relative.split("/", 1)[0]
        if first in RESERVED_ROUTES:
            return False
    return True


def _remove_partial_stage(directory: Path) -> None:
    if not directory.exists():
        return
    for current, directories, _ in os.walk(directory, topdown=True, followlinks=False):
        os.chmod(current, 0o700)
        for name in directories:
            child = Path(current) / name
            if child.is_dir() and not child.is_symlink():
                os.chmod(child, 0o700)
    shutil.rmtree(directory, ignore_errors=True)


def _scan_staged_content(
    files: list[Path], root: Path, endpoint: str, secrets: list[bytes]
) -> tuple[dict[str, str], list[dict[str, object]], int]:
    """Scan every staged file and separate rejected transport material from permitted product references."""
    endpoint_bytes = endpoint.encode()
    violations: dict[str, str] = {}
    references: list[dict[str, object]] = []
    seen_references: set[tuple[str, str]] = set()
    reference_count = 0
    for path in files:
        relative = path.relative_to(root).as_posix()
        content = path.read_bytes()
        lowered = content.lower()
        if any(identifier in lowered for identifier in PRODUCTION_IDENTIFIERS):
            violations.setdefault("productionIdentifiers", relative)
        if OFFICIAL_MT_PROTO_ROUTE.search(content):
            violations.setdefault("officialMtprotoRoutes", relative)
        if OFFICIAL_MT_PROTO_DYNAMIC_ROUTE.search(content):
            violations.setdefault("officialMtprotoDynamicRoutes", relative)
        if OFFICIAL_DC_HOST.search(content):
            violations.setdefault("officialDcHosts", relative)
        if MTPROTO_ROUTE_PATH.search(content):
            violations.setdefault("officialMtprotoRoutes", relative)
        if _is_dc_address(content):
            violations.setdefault("officialDcIpRanges", relative)
        if OFFICIAL_DC_IPV6_PREFIX.search(content):
            violations.setdefault("officialDcIpv6Prefixes", relative)
        if CLEARTEXT_WEBSOCKET.search(content):
            violations.setdefault("cleartextWebSocketRoutes", relative)
        if _has_alternate_websocket(content, endpoint_bytes):
            violations.setdefault("alternateWebSocketRoutes", relative)
        if any(marker in lowered for marker in TRUSTED_RSA_MARKER_KEYS):
            violations.setdefault("trustedRsaKeyMaterial", relative)
        if PRIVATE_KEY_BLOCK.search(content):
            violations.setdefault("privateKeyBlocks", relative)
        if any(secret and secret in content for secret in secrets):
            violations.setdefault("runSecrets", relative)
        for match in PRODUCT_REFERENCE.finditer(content):
            reference = match.group().decode("utf-8", errors="replace")
            key = (relative, reference)
            if key in seen_references:
                continue
            seen_references.add(key)
            reference_count += 1
            if len(references) < MAX_PRODUCT_REFERENCES:
                references.append({"file": relative, "reference": reference})
    return violations, references, reference_count


def _audit(directory: Path, args: argparse.Namespace) -> dict[str, object]:
    files = _artifact_files(directory)
    paths = {path.relative_to(directory).as_posix(): path for path in files}
    if "index.html" not in paths or "mtproto-target.json" not in paths:
        raise ArtifactError("artifact must contain index.html and mtproto-target.json")

    try:
        manifest = json.loads(paths["mtproto-target.json"].read_bytes())
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise ArtifactError("artifact manifest is invalid JSON") from error
    fields = {"mode", "endpoint", "fingerprint", "sourceCommit", "artifactDigest"}
    if not isinstance(manifest, dict) or set(manifest) != fields:
        raise ArtifactError("artifact manifest fields are incomplete or unexpected")

    checks: dict[str, bool] = {
        "manifestMode": manifest.get("mode") == "private",
        "manifestEndpoint": manifest.get("endpoint") == args.endpoint == ENDPOINT,
        "manifestFingerprint": manifest.get("fingerprint") == args.fingerprint,
        "manifestSourceCommit": manifest.get("sourceCommit") == args.web_revision,
        "artifactDigest": False,
        "privateCSP": False,
        "privateTargetInBundle": False,
        "safeFileTypesAndPermissions": True,
        "routeCollisions": _check_routes(files, directory),
    }
    producer_digest, digest_locale = _producer_digest(directory)
    checks["artifactDigest"] = manifest.get("artifactDigest") == producer_digest

    try:
        csp_parser = CSPParser()
        csp_parser.feed(paths["index.html"].read_text(encoding="utf-8"))
        checks["privateCSP"] = len(csp_parser.policies) == 1 and csp_parser.policies[0] == private_csp(args.endpoint)
    except (OSError, UnicodeDecodeError):
        checks["privateCSP"] = False

    all_contents = b"\n".join(path.read_bytes() for path in files)
    checks["privateTargetInBundle"] = args.endpoint.encode() in all_contents and args.fingerprint.encode() in all_contents

    secrets = _run_secrets(Path(args.secret_dir))
    violations, references, reference_count = _scan_staged_content(files, directory, args.endpoint, secrets)
    if not _check_routes(files, directory):
        checks["routeCollisions"] = False
    checks.update(dict.fromkeys(violations, False))

    failed = [name for name, passed in checks.items() if not passed]
    if failed:
        detail = ", ".join(f"{name} in {violations[name]}" if name in violations else name for name in failed)
        raise ArtifactError("artifact audit failed: " + detail)

    manifest_bytes = paths["mtproto-target.json"].read_bytes()
    index_bytes = paths["index.html"].read_bytes()
    return {
        "status": "passed",
        "webRevision": manifest["sourceCommit"],
        "endpoint": manifest["endpoint"],
        "fingerprint": manifest["fingerprint"],
        "artifactDigest": manifest["artifactDigest"],
        "stagedDigest": _digest(files, directory),
        "digestCollationLocale": digest_locale,
        "manifestSHA256": hashlib.sha256(manifest_bytes).hexdigest(),
        "indexSHA256": hashlib.sha256(index_bytes).hexdigest(),
        "fileCount": len(files),
        "totalBytes": sum(path.stat().st_size for path in files),
        "checks": checks,
        "productReferences": references,
        "productReferenceCount": reference_count,
    }


def stage(args: argparse.Namespace) -> dict[str, object]:
    if not re.fullmatch(r"[0-9a-f]{40}", args.web_revision):
        raise ArtifactError("web revision must be a full lowercase 40-character SHA")
    if not re.fullmatch(r"[0-9a-f]{16}", args.fingerprint):
        raise ArtifactError("fixture fingerprint is invalid")
    if args.endpoint != ENDPOINT:
        raise ArtifactError("fixture endpoint is invalid")

    source = _resolved(args.source, "artifact path")
    secret_directory = _resolved(args.secret_dir, "fixture secret directory")
    build_directory = _resolved(args.build_dir, "fixture build directory")
    repository_root = _resolved(args.repo_root, "server repository root")
    if not source.is_dir():
        raise ArtifactError("artifact path is not a directory")
    for protected in (secret_directory, build_directory, repository_root):
        if _overlaps(source, protected):
            raise ArtifactError("artifact source overlaps a protected fixture path")

    destination = Path(args.destination)
    if not destination.is_absolute() or destination.parent.resolve(strict=True) != build_directory or destination.exists():
        raise ArtifactError("fixture staging path is invalid")
    source_fd = _open_canonical_directory(source)
    created = False
    try:
        destination.mkdir(mode=0o700)
        created = True
        state = {"files": 0, "entries": 0, "bytes": 0}
        _copy_tree(source_fd, destination, "", state)
        os.chmod(destination, 0o555)
        report = _audit(destination, args)
        return report
    except Exception:
        if created:
            _remove_partial_stage(destination)
        raise
    finally:
        os.close(source_fd)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    stage_parser = subparsers.add_parser("stage")
    stage_parser.add_argument("--source", required=True)
    stage_parser.add_argument("--destination", required=True)
    stage_parser.add_argument("--secret-dir", required=True)
    stage_parser.add_argument("--build-dir", required=True)
    stage_parser.add_argument("--repo-root", required=True)
    stage_parser.add_argument("--endpoint", required=True)
    stage_parser.add_argument("--fingerprint", required=True)
    stage_parser.add_argument("--web-revision", required=True)
    args = parser.parse_args()
    try:
        result = stage(args)
    except (ArtifactError, OSError, UnicodeError) as error:
        print(str(error), file=sys.stderr)
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
