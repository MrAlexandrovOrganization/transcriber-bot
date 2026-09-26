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

- **Bot Service** (`bot/`) — Go bot: downloads media, submits to Whisper async queue, replies to original message
- **Whisper Service** — standalone shared service at `github.com/mrralexandrov/backends/transcriber`, running on Docker network `whisper-net`
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
- Go 1.26.7 for local development (also pinned in Docker and CI)
- Whisper backend running (see [backends/transcriber](../backends/transcriber))

The Telegram client uses `github.com/mymmrac/telego` v1.12.1 with long polling.
It supports both the public Telegram API and `LOCAL_API_URL`; with the local API,
downloaded files are read from the shared filesystem as before.

## Development checks

```bash
make install     # Install pinned buf/protobuf tools into Go's bin directory
make proto-gen   # Generate stubs from the checked-in proto, no backend checkout needed
make check       # Formatting, proto lint, unit tests and go vet
make test-race   # Run tests with the race detector
make build       # Build all Go packages
```

Make selects the exact Go 1.26.7 toolchain (downloaded automatically by Go if needed).
Ensure `$(go env GOPATH)/bin` is on PATH for tools installed by `make install`.
Telegram integration tests use a local fake API and synthetic tokens.

## Setup

1. Start the shared Whisper service first (once per machine):

```bash
# Create shared Docker network (once)
docker network create whisper-net

# Start Whisper (in backends/transcriber directory)
make up
```

2. Fill in `.env` for this project:

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

```bash
make up       # Build and start bot services
make down     # Stop bot services
make logs     # Follow bot logs
make restart  # Restart without rebuild
make deploy   # Full rebuild without cache
make proto    # Regenerate Go gRPC stubs (local dev only)
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `BOT_TOKEN` | Yes | Telegram bot token from @BotFather |
| `ROOT_ID` | Yes | Telegram user ID authorized to use the bot |
| `TELEGRAM_API_ID` | Yes* | For local Telegram Bot API server |
| `TELEGRAM_API_HASH` | Yes* | For local Telegram Bot API server |
| `WHISPER_GRPC_HOST` | No | Whisper service host (default: `whisper`) |
| `WHISPER_GRPC_PORT` | No | Whisper gRPC port (default: `50053`) |

*Required for the `telegram-bot-api` service.

## Project Structure

```
.
├── proto/
│   └── whisper.proto              # gRPC service definition (source of truth)
├── bot/                           # Go bot
│   ├── main.go
│   ├── bot/handler.go             # Telegram message handlers
│   ├── whisper/client.go          # gRPC client (Submit + GetStatus)
│   ├── config/config.go
│   ├── gen/whisper/               # Generated Go stubs (built in Dockerfile)
│   └── Dockerfile
└── docker-compose.yml             # Bot + Telegram Bot API (requires whisper-net)
```

## Connecting to Whisper from another project

See [backends/transcriber README](../backends/transcriber/README.md).
