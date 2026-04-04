package main

import (
	"log/slog"
	"os"

	"transcriber-bot/bot"
	"transcriber-bot/config"
	"transcriber-bot/whisper"
)

func main() {
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
