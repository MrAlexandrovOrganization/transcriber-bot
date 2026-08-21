package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	BotToken    string
	RootID      int64
	LocalAPIURL string
	WhisperHost string
	WhisperPort string
	DatabaseURL string
}

func Load() (*Config, error) {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("BOT_TOKEN is not set")
	}

	rootIDStr := os.Getenv("ROOT_ID")
	if rootIDStr == "" {
		return nil, fmt.Errorf("ROOT_ID is not set")
	}
	rootID, err := strconv.ParseInt(rootIDStr, 10, 64)
	if err != nil || rootID == 0 {
		return nil, fmt.Errorf("ROOT_ID must be a non-zero integer")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}

	return &Config{
		BotToken:    token,
		RootID:      rootID,
		LocalAPIURL: os.Getenv("TELEGRAM_LOCAL_API_URL"),
		WhisperHost: getEnv("WHISPER_GRPC_HOST", "localhost"),
		WhisperPort: getEnv("WHISPER_GRPC_PORT", "50053"),
		DatabaseURL: dsn,
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
