package main

import (
	"log/slog"
	"os"

	"transcriber-bot/bot"
	"transcriber-bot/config"
	"transcriber-bot/redact"
	"transcriber-bot/whisper"
)

func main() {
	// Mask the bot token in all logs, including inside error messages
	// and URLs returned by the Telegram API client.
	logger := slog.New(redact.New(slog.NewTextHandler(os.Stderr, nil), os.Getenv("BOT_TOKEN")))
	slog.SetDefault(logger)

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

	b, err := bot.New(cfg, wc)
	if err != nil {
		slog.Error("bot init", "error", err)
		os.Exit(1)
	}

	slog.Info("bot started")
	b.Run()
}
