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


def escaped_at(text, index):
    backslashes = 0
    index -= 1
    while index >= 0 and text[index] == "\\":
        backslashes += 1
        index -= 1
    return backslashes % 2 == 1


def matching_delimiter(text, start, opening, closing):
    depth = 0
    in_angle = False
    for index in range(start, len(text)):
        char = text[index]
        if escaped_at(text, index):
            continue
        if opening == "(" and char == "<":
            in_angle = True
        elif opening == "(" and char == ">":
            in_angle = False
        elif not in_angle and char == opening:
            depth += 1
        elif not in_angle and char == closing:
            depth -= 1
            if depth == 0:
                return index
    return None


def inline_destination(contents):
    contents = contents.lstrip()
    if not contents:
        return None
    if contents.startswith("<"):
        for index, char in enumerate(contents[1:], start=1):
            if char == ">" and not escaped_at(contents, index):
                return contents[1:index]
        return None

    depth = 0
    index = 0
    while index < len(contents):
        char = contents[index]
        if escaped_at(contents, index):
            index += 1
            continue
        if char == "(":
            depth += 1
        elif char == ")" and depth > 0:
            depth -= 1
        elif char.isspace() and depth == 0:
            break
        index += 1
    return contents[:index] or None


def inline_link_destinations(text):
    destinations = []
    index = 0
    while index < len(text):
        if text[index] == "!" and index + 1 < len(text) and text[index + 1] == "[" and not escaped_at(text, index):
            label_start = index + 1
        elif text[index] == "[" and not escaped_at(text, index):
            label_start = index
        else:
            index += 1
            continue

        label_end = matching_delimiter(text, label_start, "[", "]")
        if label_end is None:
            index += 1
            continue
        destination_start = label_end + 1
        if destination_start < len(text) and text[destination_start] == "(":
            destination_end = matching_delimiter(text, destination_start, "(", ")")
            if destination_end is not None:
                destination = inline_destination(text[destination_start + 1 : destination_end])
                if destination is not None:
                    destinations.append(html.unescape(destination))
                destinations.extend(inline_link_destinations(text[label_start + 1 : label_end]))
                index = destination_end + 1
                continue

        index = label_start + 1
    return destinations


def markdown_destinations(text):
    cleaned = without_code(text)
    yield from inline_link_destinations(cleaned)
    for match in REFERENCE_LINK.finditer(cleaned):
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
