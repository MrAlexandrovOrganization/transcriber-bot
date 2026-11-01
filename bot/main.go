package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"transcriber-bot/bot"
	"transcriber-bot/config"
	"transcriber-bot/internal/telemetry"
	"transcriber-bot/logx"
	"transcriber-bot/store"
	"transcriber-bot/whisper"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// JSON logs on stdout with a "service" attribute, LOG_LEVEL support,
	// and masking of the bot token in all log records.
	logx.Setup("transcriber-bot", os.Getenv("BOT_TOKEN"))
	telemetryShutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		slog.Error("telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			slog.Error("telemetry shutdown", "error", err)
		}
	}()

	// Migration CLI needs only DATABASE_URL, never Telegram credentials or polling.
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		if os.Getenv("DATABASE_URL") == "" {
			slog.Error("DATABASE_URL is required for migrate")
			os.Exit(1)
		}
		database, err := store.New(ctx, os.Getenv("DATABASE_URL"))
		if err != nil {
			slog.Error("schema migration failed")
			os.Exit(1)
		}
		database.Close()
		return
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	wc, err := whisper.NewClient(cfg.WhisperHost, cfg.WhisperPort)
	if err != nil {
		slog.Error("whisper client", "error", err)
		os.Exit(1)
	}
	defer wc.Close()

	chats, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("chat store", "error", err)
		os.Exit(1)
	}
	defer chats.Close()

	b, err := bot.New(cfg, wc, chats)
	if err != nil {
		slog.Error("bot init", "error", err)
		os.Exit(1)
	}

	slog.Info("bot started")
	if err := b.RunContext(ctx); err != nil {
		slog.Error("durable bot stopped", "reason", err)
		os.Exit(1)
	}
}
