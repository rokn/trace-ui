# trace-ui

Interactive terminal UI for exploring [Jaeger](https://www.jaegertracing.io/) distributed traces.

## Usage

```bash
go build -o trace-ui .
./trace-ui                            # connects to http://localhost:16686
./trace-ui -host http://jaeger:16686  # custom host
./trace-ui -log /tmp/trace-ui.log     # enable debug log
```

## Layout

```
┌─────────────────────┬─────────────────────────┐
│ Services            │ Operations              │
├─────────────────────┴─────────────────────────┤
│ Traces                                        │
├───────────────────────────────┬───────────────┤
│ Waterfall                     │ Span Detail   │
└───────────────────────────────┴───────────────┘
```

## Keys

| Key | Action |
|-----|--------|
| `Tab` / `Shift-Tab` | Cycle focus between panels |
| `j` / `k` | Navigate up/down in lists and waterfall |
| `Enter` | Open selected trace |
| `Space` | Collapse / expand span in waterfall |
| `h` | Toggle timing bar in waterfall |
| `r` | Refresh traces |
| `R` | Reload services |
| `/` | Filter by tag (`key=value`) |
| `c` | Config (host, limit, lookback) |
| `Esc` / `b` | Back to trace list |
| `?` | Help |
| `q` | Quit |

## Features

- Browse services and operations
- Search traces with tag filters (`http.status_code=200`, `error=true`, …)
- Configurable lookback window (15m → 7d) and result limit
- Span waterfall with collapsible tree — tree pans automatically to keep the selected span visible
- Error spans highlighted in red
- Per-service colour coding
- Span detail view: tags, logs, process tags
