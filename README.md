# Transcriber Bot

Telegram bot that transcribes audio and video messages into text using OpenAI Whisper via gRPC.

## Features

- Voice messages (OGG)
- Video notes / circles (MP4)
- Regular videos (MP4)
- Large video files sent as documents (up to 2GB via local Telegram Bot API)
- Russian language transcription
- Authorization — only the configured owner can use the bot
- Async transcription queue — bot replies immediately, edits message when done
- Whisper runs as a standalone shared service usable by multiple projects

## Architecture

```
Telegram API
     │
     ▼
 [Bot Service]  ──gRPC──►  [Whisper Service]  ◄── other projects
     │
     ▼
[Telegram Bot API Server]  (optional, for files > 20MB)
```

- **Whisper Service** (`whisper/`) — standalone, shared across projects via Docker network `whisper-net`
- **Bot Service** (`bot/`) — downloads media, submits to Whisper async queue, replies to original message
- **Telegram Bot API Server** — local server for large file support (up to 2GB)

### Async flow

```
User sends audio
       │
       ▼
Bot downloads → Submit(job) → job_id    ← returns immediately
       │
       ▼
"⏳ Расшифровываю..." (reply to original)
       │
  [poll every 5s]
       │
       ▼
Edit message → transcription text
```

### gRPC API (`proto/whisper.proto`)

| RPC | Type | Description |
|-----|------|-------------|
| `Transcribe` | client-streaming (legacy) | Blocks until done |
| `Submit` | client-streaming | Returns `job_id` immediately |
| `GetStatus` | unary | Returns job status and text |

## Prerequisites

- Docker & Docker Compose

## Setup

1. Fill in `.env`:

```bash
cp .env.example .env
```

Required variables:

```env
BOT_TOKEN=<your Telegram bot token>
ROOT_ID=<your Telegram user ID>
TELEGRAM_API_ID=<Telegram API ID>
TELEGRAM_API_HASH=<Telegram API hash>
```

## Running

### First time setup

```bash
# Create shared Docker network (once, on any machine)
docker network create whisper-net

# Start Whisper service (shared, runs independently)
make whisper-up

# Wait for model to load (~1-2 min), then start the bot
make up
```

### Daily use

```bash
make whisper-logs  # check whisper is healthy
make logs          # follow bot logs
make down          # stop bot
make whisper-down  # stop whisper
```

### Full rebuild

```bash
make whisper-deploy  # rebuild whisper without cache
make up              # rebuild bot
```

## Connecting another project to Whisper

Any project on the same machine can use the shared Whisper service:

1. Add `whisper-net` external network to its `docker-compose.yml`:

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

2. Use `proto/whisper.proto` from this repo to generate client stubs.

3. Use the `Submit` + `GetStatus` RPCs for async transcription (recommended),
   or `Transcribe` for blocking calls.

> The Whisper port is **not** published to the host — only containers in `whisper-net` can reach it.

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `BOT_TOKEN` | Yes | Telegram bot token from @BotFather |
| `ROOT_ID` | Yes | Telegram user ID authorized to use the bot |
| `TELEGRAM_API_ID` | Yes* | For local Telegram Bot API server |
| `TELEGRAM_API_HASH` | Yes* | For local Telegram Bot API server |
| `WHISPER_MODEL` | No | Model size: `tiny`, `base`, `small`, `medium`, `large` (default: `small`) |
| `GRPC_PORT` | No | Whisper gRPC port (default: `50053`) |
| `JOB_TTL_S` | No | Seconds to keep completed jobs in memory (default: `7200`) |
| `TRANSCRIBE_QUEUE_TIMEOUT_S` | No | Max wait in sync queue (default: `600`) |

*Required for the `telegram-bot-api` service.

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make whisper-up` | Build and start Whisper service |
| `make whisper-down` | Stop Whisper service |
| `make whisper-logs` | Follow Whisper logs |
| `make whisper-deploy` | Full rebuild of Whisper without cache |
| `make up` | Build and start bot services |
| `make down` | Stop bot services |
| `make logs` | Follow bot logs |
| `make restart` | Restart bot services |
| `make deploy` | Full rebuild of bot without cache |
| `make proto` | Regenerate Python gRPC stubs |
| `make proto-go` | Regenerate Go gRPC stubs (local dev) |
| `make install` | Install Python deps via Poetry |
| `make clean` | Remove Python cache files |

## Project Structure

```
.
├── proto/
│   └── whisper.proto              # gRPC service definition (source of truth)
├── whisper/
│   ├── main.py                    # gRPC server entry point
│   ├── server.py                  # Transcription service + async job queue
│   └── Dockerfile
├── bot/                           # Go bot
│   ├── main.go
│   ├── bot/handler.go             # Telegram message handlers
│   ├── whisper/client.go          # gRPC client (Submit + GetStatus)
│   ├── config/config.go
│   ├── gen/whisper/               # Generated Go stubs (built in Dockerfile)
│   └── Dockerfile
├── docker-compose.yml             # Bot + Telegram Bot API (requires whisper-net)
└── docker-compose.whisper.yml     # Standalone Whisper service (creates whisper-net)
```
