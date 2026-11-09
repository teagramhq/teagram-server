import argparse
import html
import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit


CONFIG_REFERENCE = Path("docs/configuration.md")
CONFIG_ROW = re.compile(r"^\|\s*`(TG_[A-Z0-9_]+)`\s*\|", re.MULTILINE)
GO_ENVIRONMENT_STRING = re.compile(r'(["`])(TG_[A-Z0-9]+(?:_[A-Z0-9]+)*)\1')
INLINE_LINK = re.compile(r"!?\[[^\]\n]*\]\(\s*(<[^>]+>|[^\s)]+)")
REFERENCE_LINK = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*(<[^>]+>|[^\s]+)", re.MULTILINE)
HTML_LINK = re.compile(r"(?:href|src)\s*=\s*([\"'])(.*?)\1", re.IGNORECASE)
FENCE_START = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
INLINE_CODE = re.compile(r"`+[^`]*`+")


def strip_go_comments(source):
    """Remove Go comments while keeping string literals available to scan."""
    output = []
    index = 0
    quote = None
    while index < len(source):
        char = source[index]
        following = source[index : index + 2]
        if quote is not None:
            output.append(char)
            if quote == "`":
                if char == "`":
                    quote = None
            elif char == "\\" and index + 1 < len(source):
                index += 1
                output.append(source[index])
            elif char == quote:
                quote = None
        elif following == "//":
            while index < len(source) and source[index] != "\n":
                index += 1
            if index < len(source):
                output.append("\n")
        elif following == "/*":
            index += 2
            while index < len(source) and source[index : index + 2] != "*/":
                if source[index] == "\n":
                    output.append("\n")
                index += 1
            index += 1
        else:
            if char in ('"', "'", "`"):
                quote = char
            output.append(char)
        index += 1
    return "".join(output)


def config_environment_names(root):
    names = set()
    source_dirs = (root / "internal/config", root / "internal/linklanding", root / "cmd/teagramd")
    for source_dir in source_dirs:
        if not source_dir.exists():
            continue
        for path in sorted(source_dir.rglob("*.go")):
            if path.name.endswith("_test.go"):
                continue
            if "fixture" in path.name.lower() or "perf" in path.name.lower():
                continue
            source = strip_go_comments(path.read_text(encoding="utf-8"))
            names.update(
                match.group(2)
                for match in GO_ENVIRONMENT_STRING.finditer(source)
                if not match.group(2).startswith(("TG_FIXTURE_", "TG_PERF_"))
            )
    return names


def configuration_reference_errors(root):
    reference = root / CONFIG_REFERENCE
    if not reference.is_file():
        return [f"missing configuration reference: {CONFIG_REFERENCE.as_posix()}"]

    documented = set(CONFIG_ROW.findall(reference.read_text(encoding="utf-8")))
    read_by_server = config_environment_names(root)
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
