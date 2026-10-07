# SQL query linting

Install SQLFluff in an isolated Python 3.12 environment from the root of the repository:

```sh
python3.12 -m venv .venv-sqlfluff
.venv-sqlfluff/bin/python -m pip install --require-hashes -r tools/sqlfluff-requirements.txt
```

This uses the virtual environment's pip and the fully pinned, hashed requirements file. Python 3.12 must include its `venv` and `ensurepip` components; no system pip is called. Run the read-only query-source lint with:

```sh
make lint-sql
```

The target runs `sqlfluff lint --config .sqlfluff internal/store/queries`. It uses the PostgreSQL dialect, raw templating, uppercase keyword checks (CP01), and long-line checks (LT05) at 120 columns. It checks every sqlc query source, reads no database, and needs no network after installation. The target does not run `sqlfluff fix`.

Four existing URL-matching regex literals exceed 120 columns on their own. Their exact lines carry a rule-specific `-- noqa: LT05` so the literals remain unchanged; SQLFluff still parses the statements and applies CP01. Other overlong statements continue to fail LT05.

CI runs a separate `sql lint` check with the same pinned SQLFluff requirements and `.sqlfluff` configuration. It always lints every sqlc query source. It also lints each new `.sql` migration added relative to the PR merge base. Changes or deletions to existing migration SQL fail the check; `atlas.sum` remains covered by the existing Atlas validation gate. A PR with no new migrations still lints query sources, and the migration directory itself is never passed as a lint target.

To lint a new migration locally, pass its path explicitly:

```sh
.venv-sqlfluff/bin/sqlfluff lint --config .sqlfluff migrations/<new-migration>.sql
```

SQLFluff does not lint the contents of dollar-quoted PL/pgSQL bodies, so those bodies are outside both local and CI coverage. This check is read-only: it runs `sqlfluff lint`, does not connect to a database, and does not modify generated Go bindings, migration files, or `atlas.sum`.
