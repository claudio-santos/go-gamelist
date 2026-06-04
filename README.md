# Go Gamelist

Web UI for browsing a Playnite game library exported as CSV. Single `exe`, no dependencies, ~1.5s startup for 2500+ games.

## Features

- Card and table views with sortable columns (name, developer, source, genres, release, playtime, score)
- Filters: search, genre, completion status, source (Steam/GOG/Epic/Xbox), score threshold (90+/75+/50+/25+)
- Pagination (60/page), keyboard shortcuts (`/` search, `n`/→ next, `p`/← prev), dark mode
- Lazy-reloads CSV on file changes; keeps last valid snapshot on error
- Graceful shutdown (5s timeout)

## Requirements

- **Run:** Windows 10+
- **Build:** Go 1.26+

## Quick Start

**Try it now (no Playnite export needed):**

```powershell
copy mygames.example.csv mygames.csv
go-gamelist.exe
```

**With your own library:**

1. Export your library from Playnite (Desktop plugin) → save as CSV
2. Download the latest `go-gamelist.exe` from [releases](https://github.com/claudio-santos/go-gamelist/releases)
3. Place the `.exe` and your CSV in the same folder
4. (Optional) Edit `config.yaml` to set the CSV filename or port
5. Run `go-gamelist.exe`
6. Open http://localhost:8080

## Build from Source

```powershell
git clone https://github.com/claudio-santos/go-gamelist.git
cd go-gamelist
go build -o go-gamelist.exe
```

## Configuration

`config.yaml` (optional, created with defaults on first run):

```yaml
port: 8080
csv: mygames.csv
```

Override via flags: `go-gamelist.exe -addr :3000 -csv games.csv`

## CSV Format

Standard Playnite Desktop export (~38 columns). Required columns: `Name`, `Id`. Columns used: Name, Id, Sorting Name, Description, Developers, Publishers, Genres, Platforms, Release Date, Completion Status, Time Played, Play Count, Community Score, Critic Score, User Score, Categories, Features, Game Id, Links, Sources, Regions, Series, Tags, and others.
