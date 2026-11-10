import argparse
import html
import re
import subprocess
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit


CONFIG_REFERENCE = Path("docs/configuration.md")
CONFIG_ROW = re.compile(r"^\|\s*`(TG_[A-Z0-9_]+)`\s*\|", re.MULTILINE)
ENVIRONMENT_NAME_EXTRACTOR = Path(__file__).with_name("config_environment_names.go")
INLINE_LINK = re.compile(r"!?\[[^\]\n]*\]\(\s*(<[^>]+>|[^\s)]+)")
REFERENCE_LINK = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*(<[^>]+>|[^\s]+)", re.MULTILINE)
HTML_LINK = re.compile(r"(?:href|src)\s*=\s*([\"'])(.*?)\1", re.IGNORECASE)
FENCE_START = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
INLINE_CODE = re.compile(r"`+[^`\n]*`+")


def config_environment_names(root):
    result = subprocess.run(
        ["go", "run", str(ENVIRONMENT_NAME_EXTRACTOR), "--root", str(root)],
        cwd=ENVIRONMENT_NAME_EXTRACTOR.parents[2],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or "failed to inspect Go environment reads")
    return set(result.stdout.splitlines())


def configuration_reference_errors(root):
    reference = root / CONFIG_REFERENCE
    if not reference.is_file():
        return [f"missing configuration reference: {CONFIG_REFERENCE.as_posix()}"]

    documented = set(CONFIG_ROW.findall(reference.read_text(encoding="utf-8")))
    try:
        read_by_server = config_environment_names(root)
    except RuntimeError as error:
        return [f"could not inspect server environment reads: {error}"]
    errors = [
        f"configuration reference is missing {name}"
        for name in sorted(read_by_server - documented)
    ]
    errors.extend(
        f"configuration reference lists {name}, but no non-test server code reads it"
        for name in sorted(documented - read_by_server)
    )
    return errors


def markdown_files(root):
    paths = [root / "README.md", root / "ROADMAP.md"]
    docs = root / "docs"
    if docs.is_dir():
        paths.extend(sorted(docs.glob("*.md")))
    return [path for path in paths if path.is_file()]


def without_code(text):
    lines = []
    fence_char = None
    fence_length = 0
    for line in text.splitlines():
        start = FENCE_START.match(line)
        if fence_char is None and start:
            fence_char = start.group(1)[0]
            fence_length = len(start.group(1))
            lines.append("")
            continue
        if fence_char is not None:
            closing = re.match(r"^\s{0,3}" + re.escape(fence_char) + r"{" + str(fence_length) + r",}\s*$", line)
            if closing:
                fence_char = None
                fence_length = 0
            lines.append("")
            continue
        lines.append(line)
    return INLINE_CODE.sub("", "\n".join(lines))


def markdown_destinations(text):
    cleaned = without_code(text)
    for pattern in (INLINE_LINK, REFERENCE_LINK):
        for match in pattern.finditer(cleaned):
            destination = match.group(1)
            if destination.startswith("<") and destination.endswith(">"):
                destination = destination[1:-1]
            yield html.unescape(destination)
    for match in HTML_LINK.finditer(cleaned):
        yield html.unescape(match.group(2))


def missing_markdown_links(root):
    missing = []
    for source in markdown_files(root):
        for destination in markdown_destinations(source.read_text(encoding="utf-8")):
            parsed = urlsplit(destination)
            if parsed.scheme or parsed.netloc or destination.startswith("//") or parsed.path.startswith("/"):
                continue
            target = unquote(parsed.path)
            if not target:
                continue
            resolved = source.parent / target
            if not resolved.exists():
                missing.append((source.relative_to(root).as_posix(), destination))
    return missing


def main():
    parser = argparse.ArgumentParser(description="Check configuration documentation and Markdown paths.")
    parser.add_argument("--root", type=Path, default=Path.cwd(), help="repository root")
    args = parser.parse_args()
    root = args.root.resolve()

    errors = configuration_reference_errors(root)
    errors.extend(
        f"{source} links to missing path {destination}"
        for source, destination in missing_markdown_links(root)
    )
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("Documentation checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
