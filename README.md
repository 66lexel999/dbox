# D BOX

A self-contained, segmented download manager — single ~10 MB binary, zero
dependencies (Go stdlib only), embedded web UI.

Architecture derived from [maxuanquang/idm](https://github.com/maxuanquang/idm)
(handler → logic → dataaccess layering, task lifecycle) with its distributed
stack collapsed for a local single-user app:

| reference (maxuanquang/idm) | D BOX                              |
| --------------------------- | ---------------------------------- |
| MySQL + GORM                | atomic JSON store (`tasks.json`)   |
| Kafka producer/consumer     | in-process FIFO queue + scheduler  |
| Redis cache + JWT accounts  | none — binds to localhost          |
| MinIO object storage        | direct-to-disk (`*.part` → rename) |
| gRPC + grpc-gateway         | stdlib REST + SSE                  |
| React SPA                   | embedded single-file UI            |
| single-stream `io.Copy`     | multi-connection segmented engine  |

## Features

- **Segmented downloads** — up to 32 parallel range connections per file,
  auto-sized (min 512 KB/segment)
- **Pause / resume** — segment offsets persist; resumes exactly where it
  stopped, validated with `If-Range` (ETag/Last-Modified) so a changed source
  fails loudly instead of corrupting the file
- **Crash recovery** — progress flushed every 2 s; interrupted tasks re-queue
  and continue on next launch
- **Fallbacks** — servers without range support get a single stream; chunked
  responses (unknown size) work too
- **Queue** — N tasks download at once (default 3), the rest wait
- **Global speed limit** — leaky-bucket across all connections (`-limit 2M`)
- **Per-connection retries** with exponential backoff, budget resets on progress
- **Live web UI** — per-segment progress bars, speed/ETA, SSE updates, pause /
  resume / cancel / open / show-in-folder

## Build & run

```bash
go build -tags "desktop production" -ldflags "-H windowsgui" -o bin/DBox.exe ./cmd/myidm
bin/DBox.exe
```

Opens `http://127.0.0.1:8081`. Files land in `%USERPROFILE%\Downloads\flowerX`,
state in `%LOCALAPPDATA%\flowerX\tasks.json`.

```
-listen 127.0.0.1:8081   UI/API address
-dir <path>              download directory
-data <path>             state directory
-segments 8              default connections per download (1-32)
-concurrent 3            max simultaneous downloads
-limit 0                 global speed cap (e.g. 2M, 500K; 0 = unlimited)
-retries 5               retries per connection
-ua <string>             User-Agent
-open=true               open browser on start
```

## API

| Method | Path                    | Description                            |
| ------ | ----------------------- | -------------------------------------- |
| GET    | `/api/tasks`            | list tasks (newest first)              |
| POST   | `/api/tasks`            | `{"url":"…","fileName":"…","segments":8}` |
| GET    | `/api/tasks/{id}`       | one task                               |
| POST   | `/api/tasks/{id}/pause` | pause (keeps segment progress)         |
| POST   | `/api/tasks/{id}/resume`| resume / retry                         |
| POST   | `/api/tasks/{id}/schedule`| `{"at":<epoch ms>}` start later; `0` cancels |
| DELETE | `/api/tasks/{id}?file=1`| remove entry (`file=1` deletes data)   |
| GET    | `/api/tasks/{id}/file`  | stream the completed file              |
| POST   | `/api/tasks/{id}/reveal`| select file in Explorer                |
| GET    | `/api/events`           | SSE: full task snapshot every 500 ms   |

## Layout

```
cmd/myidm/            entrypoint, flags, graceful shutdown
internal/config/      flag parsing, defaults
internal/store/       atomic JSON persistence
internal/engine/      scheduler, segment planner, ranged/whole-stream
                      downloaders, rate limiter, probe (size/ranges/filename)
internal/server/      REST + SSE handlers, embedded web UI
tools/testserver/     HTTP server simulating ranged/slow/no-range/chunked origins
tools/e2e.sh          end-to-end suite (integrity, pause/resume, crash recovery)
```

## Tests

```bash
go build -o bin/testserver.exe ./tools/testserver
bash tools/e2e.sh
```

21 checks: SHA256 integrity across 6-connection downloads, pause/resume,
no-range fallback, chunked transfer, hard-kill crash recovery, real-world URL.
