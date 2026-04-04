# AGENTS.md

Guidance for agentic coding agents working in this repository.

## Project Overview

**Essence** is a Quebec gas price heatmap — a minimal, self-contained web application with two source files:

- `main.go` — the entire Go backend (HTTP server, SQLite persistence, upstream polling)
- `static/index.html` — the entire frontend (Leaflet map, Chart.js charts, vanilla JS)

Keep this architecture. Do not split `main.go` into multiple files or introduce a frontend build step unless explicitly asked.

## Environment Setup

The dev environment is managed with Nix flakes. Enter it with either:

```sh
nix develop        # explicit
direnv allow       # automatic via .envrc (uses `use flake`)
```

The shell provides: `go`, `gopls`, `gotools` (includes `gofmt`, `goimports`).

## Build & Run

```sh
# Build binary
go build -o essence .

# Run directly (no build step needed for development)
go run .

# With explicit env vars (both have defaults)
PORT=8080 ESSENCE_DB=./essence.db go run .

# Build via Nix
nix build
./result/bin/essence
```

Environment variables:
- `PORT` — HTTP listen port (default: `8080`)
- `ESSENCE_DB` — SQLite database file path (default: `./essence.db`)

## Lint & Format

```sh
gofmt -w .          # format all Go files (authoritative formatter — no config)
goimports -w .      # format + organize imports (superset of gofmt)
go vet ./...        # static analysis
```

There are no linter config files. Follow standard Go formatting conventions enforced by `gofmt`.

## Tests

```sh
# Run all tests
go test ./...

# Run a single test by name
go test -run TestFunctionName .

# Run with verbose output
go test -v -run TestFunctionName .
```

No tests exist yet. When adding tests, use Go's built-in `testing` package — no third-party test libraries. Place test files alongside the code they test (e.g., `main_test.go`).

## Go Code Style

### Imports

Group imports into two blocks separated by a blank line: stdlib first, then third-party. This is enforced by `goimports`.

```go
import (
    "database/sql"
    "encoding/json"
    "fmt"
    "log"
    "net/http"

    _ "modernc.org/sqlite"
)
```

### Naming

- Exported types, functions, fields: `PascalCase`
- Unexported variables, functions: `camelCase`
- Acronyms follow Go convention: `geojsonURL`, `initDB`, `handleStations`

### Error Handling

Always check errors immediately. Wrap errors with context using `%w`. Use `log.Fatalf` for unrecoverable startup errors; `log.Printf` + early return for runtime errors.

```go
// Startup — fatal is appropriate
db, err = initDB(dbPath)
if err != nil {
    log.Fatalf("db init: %v", err)
}

// Wrapping with context
return nil, fmt.Errorf("create table: %w", err)
return nil, fmt.Errorf("fetching data: %w", err)

// Runtime — log and return, do not crash
if err != nil {
    log.Printf("fetch error: %v", err)
    return
}
```

Never silently discard errors.

### Types & Structs

Use struct-based JSON serialization with `json` tags. Use anonymous inline structs for one-off local parsing; define named types for anything reused or returned from functions.

```go
type Station struct {
    Name    string  `json:"name"`
    Brand   string  `json:"brand"`
    Lat     float64 `json:"lat"`
    Regular float64 `json:"regular"`
}

// Inline anonymous struct for local, single-use parsing
var features []struct {
    Geometry struct {
        Coordinates [2]float64 `json:"coordinates"`
    } `json:"geometry"`
}
```

### Constants

Group related constants in a single `const` block.

```go
const (
    geojsonURL   = "https://example.com/stations.geojson.gz"
    defaultPort  = "8080"
    pollInterval = 5 * time.Minute
)
```

### Concurrency

Use `sync.RWMutex` to protect shared state. Acquire the narrowest lock needed.

```go
var (
    cacheMu    sync.RWMutex
    cachedResp *StationsResponse
)

cacheMu.Lock()
cachedResp = resp
cacheMu.Unlock()

cacheMu.RLock()
resp := cachedResp
cacheMu.RUnlock()
```

### Comments

Write doc-style comments on all exported types and non-trivial unexported functions. Keep comments concise and sentence-cased.

```go
// Station holds the parsed data for a single fuel station.
type Station struct { ... }

// poller fetches upstream data immediately, then repeats every pollInterval.
func poller() { ... }
```

## Frontend Code Style (`static/index.html`)

- **No framework, no build step.** Plain HTML, CSS, and JavaScript in a single file.
- **Inline** `<style>` and `<script>` blocks — no external local JS/CSS files.
- **CDN** dependencies only: Leaflet, leaflet.markercluster, Chart.js, `@knadh/oat` CSS.
- **`camelCase`** for all JS variables and function names.
- **`fetch()` with `.then()` promise chains** — not `async/await`.
- **CSS custom properties** (`--primary`, `--border`, etc.) from the `oat` design token system.
- **French language** — all user-facing text must be in French (`lang="fr"` on `<html>`).

## Architecture Notes

- Static assets are embedded into the binary at compile time via `//go:embed static/*`. No file serving from disk at runtime.
- The SQLite driver is `modernc.org/sqlite` — a pure-Go, CGo-free implementation. Do not introduce CGo or replace this driver.
- Deployment target is a NixOS systemd service defined in `nixos-module.nix`.
- The module `github.com/polen/essence` targets Go 1.25+.
