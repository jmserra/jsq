# jsq

A minimal, opinionated, **vim-style terminal UI for SQL databases** —
PostgreSQL, MySQL, and SQLite. One static Go binary, keyboard-only, no mouse.

jsq is heavily inspired by [lazysql](https://github.com/jorgerojas26/lazysql).
It's a deliberately smaller, more opinionated take on the same idea: fast,
keyboard-driven database browsing, with SQL authoring and heavier edits handed
off to your real `$EDITOR`. See [DESIGN.md](DESIGN.md) for the full design and
how the two relate.

## Status

Browsing works end-to-end across all three engines: connect, list tables, and a
fixed-width results grid with continuous scroll, per-column sort, per-column
filter, and a full-cell viewer. **Editing (`e`/`E`/`o`/`D`/`p`), the `$EDITOR`
query flow, query history, and the help overlay are on the roadmap** (see below).

## Install

```sh
# from source
git clone https://github.com/jmserra/jsq && cd jsq
go build -o jsq .
# or
go install github.com/jmserra/jsq@latest
```

No cgo — SQLite is pure-Go (`modernc.org/sqlite`), so cross-compilation is trivial.

## Usage

```sh
jsq <name>                     # connect using a named entry in the config file
jsq                            # no args → interactive connection picker
jsq postgres://user@host/db    # ad-hoc: connect straight to a URL
jsq ./local.db                 # ad-hoc: a file path → SQLite
jsq -c ~/other.toml <name>     # use a different connections file
```

The engine is inferred from the URL scheme (`postgres`/`postgresql`, `mysql`,
`sqlite`/`file`) or a bare file path (SQLite).

## Connections file

Location: `$JSQ_CONFIG` or `~/.config/jsq/connections.toml`. **jsq only ever
reads this file** — there is no in-app connection editor. One section per
connection; the section header *is* the connection name.

```toml
[demo]
url = "sqlite://./demo.db"

[local]
url = "postgres://user@localhost:5432/mydb?sslmode=disable"

[work]
url = "mysql://user@localhost:3306/mydb"
env = "JSQ_WORK_PASSWORD"   # password read from this env var at connect time
read_only = true            # optional: refuse all mutations on this connection
```

Put the password in the URL, or point at an env var with `env = "..."` so the
file can live in your dotfiles without a secret in it.

## Keybindings

| Key | Action |
| --- | --- |
| `h` `j` `k` `l` | move cursor |
| `g` / `G` | first / last row |
| `0` / `$` | first / last column |
| `J` / `K` | sort current column ascending / descending |
| `/` | filter current column (type to preview; `↑`/`↓` browse matches) |
| `Enter` (grid) | commit filter, or — with no filter — inspect the full cell value |
| `Esc` | clear the current column's filter |
| `H` | toggle the table sidebar (focuses it; auto-hides on select) |
| `Enter` (sidebar) | open the selected table |
| `Tab` / `Shift-Tab` | cycle focus between sidebar and grid |
| `Ctrl-c` | quit |

**Filtering** is prefix-search by default (a trailing `%` is added
automatically); type a leading `%` yourself for a contains match. Filters are
case-insensitive, work on any column type, and stack across columns. **Scrolling
is continuous** — reaching the loaded edge fetches the next window; there are no
pages. The grid opens sorted by primary key, newest first.

## Roadmap

- **Editing** — `e` quick cell edit, `E`/`o`/`D`/`p` (edit / insert / delete /
  duplicate) generated as SQL and opened in `$EDITOR`
- **`s` / `S`** — author free-form SQL in `$EDITOR`
- **Query history** — `Ctrl-r` picker and `Ctrl-o` step-back
- **`?` help overlay** — generated from the keymap
- **Sidebar filter**, **clipboard yank** (`y`/`Y`), and a **database picker**

## Credits

Enormous thanks to [lazysql](https://github.com/jorgerojas26/lazysql), which
inspired jsq's shape and interaction model. If you want a broader, more
full-featured database TUI, use lazysql — it's excellent.
