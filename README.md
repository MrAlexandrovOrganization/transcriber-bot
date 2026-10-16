# Transcriber Bot

## Build versions

Go and protoc-gen-go come from `bot/go.mod`; remaining tools and images live
in `versions.mk`. `make versions` prints values, `make install` installs the
pinned tools, and `make compose-build` builds images with Make-provided args.
CI reads Go from `bot/go.mod`. Re-run installation after changing tool versions.

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
- Go from `bot/go.mod` for local development, Docker and CI
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

Make selects the Go toolchain from `bot/go.mod` (downloaded automatically if needed).
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

## Доставка образов из CI

Рекомендуемый путь релиза: собирать образ после проверок в CI и разворачивать
опубликованный образ по digest. На VM не пересобирать приложение из checkout:
так запускается тот же артефакт, который прошёл проверки.

CI выполняет существующие проверки, затем `make image-build` собирает все собственные
образы из build-секций Compose без runtime-секретов и соседних checkout.
Push main публикует **эти же** образы в GHCR через `make image-publish`.
Тег содержит SHA, run ID и attempt; artifact `image-release` содержит digest manifest.
Платформа — `linux/amd64` (текущая VM). На PR публикации и доставки нет.
Версии build args по-прежнему приходят из Make, определения сборки — из Compose.

SSH CD передаёт SHA, manifest и временный token в `make ci-deploy`.
Эта цель из `makefiles/ci-deploy.mk` под одной блокировкой проверяет чистый main,
отклоняет устаревший workflow, fast-forward обновляет checkout до проверенного SHA,
временно авторизует Docker в GHCR и вызывает `make image-deploy` из нового Makefile.
Сначала скачиваются все образы, затем `image-activate` сохраняет подготовку/миграции
этого проекта и запускает Compose с `--no-build --pull never --wait`.
Доступ к registry удаляется и при ошибке. `ci-deploy` требует переменные
`DEPLOY_SHA`, `IMAGE_MANIFEST_B64`, `GHCR_USER`, `GHCR_TOKEN` из окружения;
не передавайте token аргументом Make или команды.

Короткий SSH-шаг делает fetch и загружает только `makefiles/ci-deploy.mk` через
`git show` из SHA этого workflow. Поэтому первый деплой работает и со старым
checkout без новой цели. Загруженный файл сам проверяет актуальность SHA и повторно
делает fetch после захвата блокировки; параллельные доставки не обновляют checkout
во время активации. Обычные `make up` и существующие `make deploy` сохраняют
свои прежние контракты; для CI служит отдельная цель `ci-deploy`.

Для CI разрешите GitHub Actions запись Packages. Новые GHCR packages по умолчанию
private; настройте наследование доступа от этого репозитория. Автоматический CD
использует временный `GITHUB_TOKEN` с `packages: read` и временный Docker config,
который удаляется после SSH-шагa. Для ручной доставки нужен отдельный read-only
доступ (`read:packages`) или публичный package; приложение секреты получает только на VM.
VM нужны Python 3.9+, Make, Docker Compose с `--wait`, Git и flock для SSH CD.

`.images/current.json` хранит последний успешный manifest, `previous.json` — предыдущий.
Для отката сначала согласуйте совместимость БД и конфигурации, верните checkout
к `revision` выбранного manifest и выполните `make image-deploy IMAGE_MANIFEST=...`.
Автоматического отката БД/образов нет; не удаляйте нужные digests в GHCR.
`make up` остаётся локальной сборкой из исходников: на VM для релиза используйте
`image-deploy`; `logs`/`ps` читают те же контейнеры. Сторонние образы не пересобираются,
их версии остаются в конфигурации проекта. Внешний registry-кеш между CI runs пока
не настроен. Успешный build не подтверждает runtime: проверяйте readiness и сценарий
сервиса после доставки.

Для диагностики и явных CLI-операций из активного образа используйте
`make image-compose IMAGE_ARGS='ps'`, `IMAGE_ARGS='logs -f'` или
`IMAGE_ARGS='run --rm SERVICE COMMAND'`. Последняя команда автоматически
запрещает сборку и pull. Она подходит для явных миграций/CLI, которые раньше
запускались через локальный Compose-образ. После неуспешной активации
`.images/requested.json` сохраняет manifest попытки; `current.json` остаётся
последним успешным и не гарантирует, что частичный rollout откатился.

Ручной `image-deploy` использует тот же host lock, что SSH CD; не запускайте
обходные Compose-операции одновременно с доставкой. При первом переходе на GHCR
`previous.json` ещё отсутствует: сохраните прежние локальные образы и конфигурацию
для восстановления по runbook до первого релиза.

## Container names

Compose uses explicit service names without replica numbers. Set
`CONTAINER_NAME_PREFIX` (for example `dev-`) for an additional local or CI stack
on the same Docker host. Keep Compose project/service labels and volume names
unchanged; clients address services or explicit network aliases.
