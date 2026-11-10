import argparse
import html
import re
import string
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
ATX_HEADING = re.compile(r"^ {0,3}#{1,6}(?:[ \t]+|$)")
SETEXT_HEADING = re.compile(r"^ {0,3}(?:=+|-+)\s*$")
THEMATIC_BREAK = re.compile(r"^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$")
LIST_ITEM_START = re.compile(r"^( {0,3})(?:[-+*]|[0-9]{1,9}[.)])([ \t]+|$)")
BLOCKQUOTE_START = re.compile(r"^ {0,3}>")
TABLE_DELIMITER_CELL = re.compile(r":?-{3,}:?$")


def indentation_columns(line, columns=0):
    for char in line:
        if char == " ":
            columns += 1
        elif char == "\t":
            columns += 4 - columns % 4
        else:
            break
    return columns


def indented_code_line(line, minimum=4):
    return indentation_columns(line) >= minimum


def remove_indentation_columns(line, columns):
    consumed = 0
    index = 0
    while index < len(line) and consumed < columns:
        if line[index] == " ":
            consumed += 1
        elif line[index] == "\t":
            next_column = consumed + 4 - consumed % 4
            if next_column > columns:
                return " " * (next_column - columns) + line[index + 1 :]
            consumed = next_column
        else:
            break
        index += 1
    return line[index:]


def list_item_indents(line):
    match = LIST_ITEM_START.match(line)
    if match is None:
        return None

    marker_start = match.end(1)
    padding_start = match.start(2)
    marker_width = len(line[marker_start:padding_start])
    marker_indent = indentation_columns(match.group(1))
    content_indent = indentation_columns(match.group(2), marker_indent + marker_width)
    return marker_indent, content_indent


def strip_blockquote_prefixes(line):
    while match := BLOCKQUOTE_START.match(line):
        line = line[match.end() :]
        if line.startswith((" ", "\t")):
            line = line[1:]
    return line


def table_row_cells(line, container_indent=0):
    indentation = indentation_columns(line)
    if indentation < container_indent or indentation - container_indent > 3:
        return None
    line = remove_indentation_columns(line, container_indent)
    line = strip_blockquote_prefixes(line)
    if indentation_columns(line) > 3:
        return None
    line = line.strip()
    cells = []
    cell_start = 0
    for index, char in enumerate(line):
        if char == "|" and not escaped_at(line, index):
            cells.append(line[cell_start:index].strip())
            cell_start = index + 1
    if not cells:
        return None

    cells.append(line[cell_start:].strip())
    if line.startswith("|"):
        cells.pop(0)
    if line.endswith("|") and not escaped_at(line, len(line) - 1):
        cells.pop()
    return cells


def gfm_table_line_starts(text):
    lines = []
    line_start = 0
    while line_start < len(text):
        line_end = text.find("\n", line_start)
        if line_end == -1:
            line_end = len(text)
        else:
            line_end += 1
        content_end = line_end
        if content_end > line_start and text[content_end - 1] == "\n":
            content_end -= 1
        if content_end > line_start and text[content_end - 1] == "\r":
            content_end -= 1
        lines.append((line_start, text[line_start:content_end]))
        line_start = line_end

    container_indents = []
    list_stack = []
    for _, line in lines:
        if line.strip():
            list_indents = list_item_indents(line)
            if list_indents is not None:
                marker_indent, _ = list_indents
                while list_stack and marker_indent <= list_stack[-1][0]:
                    list_stack.pop()
                list_stack.append(list_indents)
            else:
                indentation = indentation_columns(line)
                while list_stack and indentation < list_stack[-1][1]:
                    list_stack.pop()
        container_indents.append(list_stack[-1][1] if list_stack else 0)

    table_lines = set()
    index = 0
    while index + 1 < len(lines):
        header_cells = table_row_cells(lines[index][1], container_indents[index])
        delimiter_cells = table_row_cells(lines[index + 1][1], container_indents[index + 1])
        if (
            header_cells is None
            or delimiter_cells is None
            or container_indents[index] != container_indents[index + 1]
            or len(header_cells) != len(delimiter_cells)
            or not all(TABLE_DELIMITER_CELL.fullmatch(cell) for cell in delimiter_cells)
        ):
            index += 1
            continue

        table_lines.update((lines[index][0], lines[index + 1][0]))
        index += 2
        while index < len(lines) and table_row_cells(lines[index][1], container_indents[index]) is not None:
            table_lines.add(lines[index][0])
            index += 1
    return table_lines


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
    characters = list(text)
    block_boundaries = set()
    table_lines = gfm_table_line_starts(text)
    fence_char = None
    fence_length = 0
    in_blockquote = False
    in_indented_code = False
    allow_indented_code = True
    last_block_was_list_item = False
    list_content_indent = 0
    indented_code_indent = 4
    line_start = 0
    while line_start < len(text):
        line_end = text.find("\n", line_start)
        if line_end == -1:
            line_end = len(text)
        else:
            line_end += 1
        content_end = line_end
        if content_end > line_start and text[content_end - 1] == "\n":
            content_end -= 1
        if content_end > line_start and text[content_end - 1] == "\r":
            content_end -= 1
        line = text[line_start:content_end]
        if fence_char is not None:
            closing = re.match(r"^\s{0,3}" + re.escape(fence_char) + r"{" + str(fence_length) + r",}\s*$", line)
            if closing:
                fence_char = None
                fence_length = 0
                block_boundaries.add(line_end)
                in_blockquote = False
                allow_indented_code = True
                last_block_was_list_item = False
            for index in range(line_start, content_end):
                characters[index] = " "
        elif in_indented_code and not line.strip():
            block_boundaries.update((line_start, line_end))
        elif in_indented_code and indented_code_line(line, indented_code_indent):
            for index in range(line_start, content_end):
                characters[index] = " "
        else:
            if in_indented_code:
                in_indented_code = False
                indented_code_indent = 4
                block_boundaries.add(line_start)

            start = FENCE_START.match(line)
            if start:
                block_boundaries.add(line_start)
                in_blockquote = False
                last_block_was_list_item = False
                fence_char = start.group(1)[0]
                fence_length = len(start.group(1))
                allow_indented_code = False
                for index in range(line_start, content_end):
                    characters[index] = " "
            elif not line.strip():
                block_boundaries.update((line_start, line_end))
                in_blockquote = False
                allow_indented_code = True
            elif line_start in table_lines:
                block_boundaries.update((line_start, line_end))
                in_blockquote = False
                allow_indented_code = False
                last_block_was_list_item = False
            elif allow_indented_code and indented_code_line(
                line,
                list_content_indent + 4 if last_block_was_list_item else 4,
            ):
                block_boundaries.add(line_start)
                in_indented_code = True
                indented_code_indent = list_content_indent + 4 if last_block_was_list_item else 4
                in_blockquote = False
                allow_indented_code = False
                for index in range(line_start, content_end):
                    characters[index] = " "
            elif BLOCKQUOTE_START.match(line):
                if not in_blockquote:
                    block_boundaries.add(line_start)
                in_blockquote = True
                allow_indented_code = False
                last_block_was_list_item = False
            elif ATX_HEADING.match(line) or SETEXT_HEADING.match(line) or THEMATIC_BREAK.match(line) or REFERENCE_LINK.match(line):
                block_boundaries.update((line_start, line_end))
                in_blockquote = False
                allow_indented_code = True
                last_block_was_list_item = False
            elif (list_indents := list_item_indents(line)) is not None:
                block_boundaries.add(line_start)
                in_blockquote = False
                allow_indented_code = False
                last_block_was_list_item = True
                list_content_indent = list_indents[1]
            else:
                allow_indented_code = False
                if not line.startswith((" ", "\t")):
                    last_block_was_list_item = False
        line_start = line_end

    cleaned = "".join(characters)
    characters = list(cleaned)
    index = 0
    while index < len(cleaned):
        if cleaned[index] != "`":
            index += 1
            continue
        if escaped_at(cleaned, index):
            index += 1
            continue

        run_end = index + 1
        while run_end < len(cleaned) and cleaned[run_end] == "`":
            run_end += 1
        run_length = run_end - index
        search = run_end
        closing_end = None
        while search < len(cleaned):
            if search in block_boundaries:
                break
            if cleaned[search] != "`":
                search += 1
                continue
            candidate_end = search + 1
            while candidate_end < len(cleaned) and cleaned[candidate_end] == "`":
                candidate_end += 1
            if candidate_end - search == run_length:
                closing_end = candidate_end
                break
            search = candidate_end

        if closing_end is None:
            index = run_end
            continue
        for span_index in range(index, closing_end):
            if characters[span_index] not in "\r\n":
                characters[span_index] = " "
        index = closing_end

    return "".join(characters)


def escaped_at(text, index):
    if text[index] not in string.punctuation:
        return False
    backslashes = 0
    index -= 1
    while index >= 0 and text[index] == "\\":
        backslashes += 1
        index -= 1
    return backslashes % 2 == 1


def unescape_markdown(text):
    unescaped = []
    index = 0
    while index < len(text):
        if text[index] == "\\" and index + 1 < len(text) and text[index + 1] in string.punctuation:
            unescaped.append(text[index + 1])
            index += 2
        else:
            unescaped.append(text[index])
            index += 1
    return "".join(unescaped)


def line_ending_end(text, index):
    if text[index] == "\r":
        return index + 2 if index + 1 < len(text) and text[index + 1] == "\n" else index + 1
    if text[index] == "\n":
        return index + 1
    return None


def starts_blank_line(text, index):
    next_index = line_ending_end(text, index)
    if next_index is None:
        return False
    while next_index < len(text) and text[next_index] in " \t":
        next_index += 1
    return next_index < len(text) and line_ending_end(text, next_index) is not None


def matching_link_parenthesis(text, start):
    index = start + 1
    while index < len(text) and text[index].isspace():
        index += 1

    if index >= len(text):
        return None

    if text[index] == "<" and not escaped_at(text, index):
        index += 1
        while index < len(text):
            if text[index] in "\r\n":
                return None
            if text[index] == ">" and not escaped_at(text, index):
                break
            index += 1
        if index >= len(text):
            return None
        index += 1
    else:
        depth = 0
        while index < len(text):
            char = text[index]
            if escaped_at(text, index):
                index += 1
                continue
            if char.isspace():
                break
            if char == "(":
                depth += 1
            elif char == ")":
                if depth == 0:
                    return index
                depth -= 1
            index += 1

    had_whitespace = False
    while index < len(text) and text[index].isspace():
        if starts_blank_line(text, index):
            return None
        had_whitespace = True
        index += 1
    if index >= len(text):
        return None
    if text[index] == ")" and not escaped_at(text, index):
        return index
    if not had_whitespace:
        return None

    title_opening = text[index]
    if title_opening in "\"'":
        index += 1
        while index < len(text):
            if starts_blank_line(text, index):
                return None
            if text[index] == title_opening and not escaped_at(text, index):
                index += 1
                break
            index += 1
        else:
            return None
    elif title_opening == "(":
        depth = 1
        index += 1
        while index < len(text) and depth:
            if starts_blank_line(text, index):
                return None
            if escaped_at(text, index):
                index += 1
                continue
            if text[index] == "(":
                return None
            elif text[index] == ")":
                depth -= 1
            index += 1
        if depth:
            return None
    else:
        return None

    while index < len(text) and text[index].isspace():
        index += 1
    if index < len(text) and text[index] == ")" and not escaped_at(text, index):
        return index
    return None


def matching_delimiter(text, start, opening, closing):
    if opening == "(" and closing == ")":
        return matching_link_parenthesis(text, start)

    depth = 1
    for index in range(start + 1, len(text)):
        char = text[index]
        if escaped_at(text, index):
            continue
        if char == opening:
            depth += 1
        elif char == closing:
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


def inline_link_destinations(text, base_offset=0, parse_errors=None):
    if parse_errors is None:
        parse_errors = []
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
            if destination_end is None:
                parse_errors.append((
                    base_offset + destination_start,
                    base_offset + label_start,
                    base_offset + label_end + 1,
                ))
                destinations.extend(
                    inline_link_destinations(
                        text[label_start + 1 : label_end],
                        base_offset + label_start + 1,
                        parse_errors,
                    )
                )
                index = destination_start + 1
                continue
            destination = inline_destination(text[destination_start + 1 : destination_end])
            if destination is not None:
                destinations.append(html.unescape(unescape_markdown(destination)))
            destinations.extend(
                inline_link_destinations(
                    text[label_start + 1 : label_end],
                    base_offset + label_start + 1,
                    parse_errors,
                )
            )
            index = destination_end + 1
            continue

        index = label_start + 1
    return destinations


def markdown_destinations(text, parse_errors=None):
    cleaned = without_code(text)
    yield from inline_link_destinations(cleaned, parse_errors=parse_errors)
    for match in REFERENCE_LINK.finditer(cleaned):
        destination = match.group(1)
        if destination.startswith("<") and destination.endswith(">"):
            destination = destination[1:-1]
        yield html.unescape(unescape_markdown(destination))
    for match in HTML_LINK.finditer(cleaned):
        yield html.unescape(match.group(2))


def missing_markdown_links(root):
    errors = []
    for source in markdown_files(root):
        text = source.read_text(encoding="utf-8")
        relative_source = source.relative_to(root).as_posix()
        parse_errors = []
        for destination in markdown_destinations(text, parse_errors):
            parsed = urlsplit(destination)
            if parsed.scheme or parsed.netloc or destination.startswith("//") or parsed.path.startswith("/"):
                continue
            target = unquote(parsed.path)
            if not target:
                continue
            resolved = source.parent / target
            if not resolved.exists():
                errors.append(f"{relative_source} links to missing path {destination}")
        for offset, label_start, label_end in parse_errors:
            line = text.count("\n", 0, offset) + 1
            link_text = text[label_start:label_end]
            errors.append(f"{relative_source}:{line} has an unclosed Markdown link {link_text}")
    return errors


def main():
    parser = argparse.ArgumentParser(description="Check configuration documentation and Markdown paths.")
    parser.add_argument("--root", type=Path, default=Path.cwd(), help="repository root")
    args = parser.parse_args()
    root = args.root.resolve()

    errors = configuration_reference_errors(root)
    errors.extend(missing_markdown_links(root))
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("Documentation checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
