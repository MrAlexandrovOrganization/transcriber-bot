# CLAUDE.md

## Project Overview

Python Telegram bot that transcribes audio/video messages using OpenAI Whisper. Multi-service architecture: bot (Go) + whisper gRPC server (Python) + optional local Telegram API server.

**Languages:** Go 1.26 (bot), Python 3.11+ (whisper server)
**Package manager:** Poetry (`pyproject.toml`) for Python; Go modules for bot
**Deployment:** Docker Compose (two separate compose files)

## Key Files

| File | Purpose |
|------|---------|
| `bot/bot/handler.go` | Telegram handlers — voice, video, document messages; async poll loop |
| `bot/whisper/client.go` | gRPC client — Submit (async) + GetStatus |
| `bot/config/config.go` | Loads env vars, validates `BOT_TOKEN` and `ROOT_ID` |
| `whisper/server.py` | Whisper model runner — async job queue, one job at a time |
| `whisper/main.py` | gRPC server entry point |
| `proto/whisper.proto` | gRPC service definition — source of truth for all clients |
| `docker-compose.yml` | Bot + telegram-bot-api services (requires external `whisper-net`) |
| `docker-compose.whisper.yml` | Standalone Whisper service (creates `whisper-net`) |
| `Makefile` | All dev and deploy commands |

## Running & Building

```bash
# First time
docker network create whisper-net
make whisper-up    # start whisper (wait ~1-2 min for model load)
make up            # start bot

# Daily
make logs          # bot logs
make whisper-logs  # whisper logs
make down          # stop bot
make whisper-down  # stop whisper

# Rebuild
make whisper-deploy  # whisper full rebuild --no-cache
make up              # bot rebuild
make proto           # regenerate Python stubs
make proto-go        # regenerate Go stubs (local dev only, Docker builds them)
```

## Architecture Notes

- Whisper is a **standalone shared service** on Docker network `whisper-net` — not published to host
- Bot submits audio via `Submit` RPC → gets `job_id` immediately → polls `GetStatus` every 5s
- Bot replies to the original message with status, edits it when done
- Whisper processes jobs **one at a time** (single background thread) — CPU-bound
- Completed jobs stored in memory for 2h then cleaned up
- gRPC max message size: **50MB**; Telegram local API handles files up to **2GB**
- Bot uses **polling** (not webhooks)
- Authorization: only user with `ROOT_ID` triggers transcription

## Proto / gRPC API

`proto/whisper.proto` is the source of truth for all clients connecting to the shared whisper service.

| RPC | Type | Description |
|-----|------|-------------|
| `Transcribe` | client-streaming (legacy) | Blocks until done |
| `Submit` | client-streaming | Returns `job_id` + queue position immediately |
| `GetStatus` | unary | Returns `PENDING/RUNNING/DONE/FAILED` + text |

When changing `proto/whisper.proto`, regenerate stubs:

```bash
make proto      # Python stubs (also regenerated in Docker build)
make proto-go   # Go stubs (also regenerated in bot/Dockerfile)
```

Generated files — do not edit manually:
- `proto/whisper_pb2.py`
- `proto/whisper_pb2_grpc.py`
- `bot/gen/whisper/` (generated during Docker build, empty in repo)

## Connecting Another Project

Add to its `docker-compose.yml`:

```yaml
networks:
  whisper-net:
    external: true

services:
  your-service:
    networks:
      - whisper-net
    environment:
      - WHISPER_GRPC_HOST=whisper
      - WHISPER_GRPC_PORT=50053
```

Use `proto/whisper.proto` from this repo to generate client stubs for any language.

## Environment Variables

Required in `.env`:
- `BOT_TOKEN` — Telegram bot token
- `ROOT_ID` — authorized Telegram user ID
- `TELEGRAM_API_ID`, `TELEGRAM_API_HASH` — for local Telegram Bot API server

Optional (set in compose files):
- `WHISPER_MODEL` — model size (default `small` in whisper compose)
- `GRPC_PORT` — default 50053
- `WHISPER_GRPC_HOST` / `WHISPER_GRPC_PORT` — bot → whisper connection
- `JOB_TTL_S` — seconds to keep job results in memory (default 7200)

## Common Pitfalls

- Use `docker compose` (not `docker-compose`) — Makefile uses the new syntax
- `whisper-net` must be created before `make up`: `docker network create whisper-net`
- Whisper must be started before the bot: `make whisper-up` then `make up`
- Whisper model download happens at first container start — first `make whisper-up` is slow
- Large files require `telegram-bot-api` service; without it Telegram limits downloads to ~20MB
- Go stubs in `bot/gen/whisper/` are intentionally empty in the repo — generated at Docker build time
