package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"transcriber-bot/bot"
	"transcriber-bot/config"
	"transcriber-bot/internal/telemetry"
	"transcriber-bot/logx"
	"transcriber-bot/store"
	"transcriber-bot/whisper"
)

func main() {
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

	chats, err := store.New(context.Background(), cfg.DatabaseURL)
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
	b.Run()
}
