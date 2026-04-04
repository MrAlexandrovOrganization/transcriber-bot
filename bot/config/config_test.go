package config

import (
	"testing"
)

func TestLoad_OK(t *testing.T) {
	t.Setenv("BOT_TOKEN", "token123")
	t.Setenv("ROOT_ID", "42")
	t.Setenv("TELEGRAM_LOCAL_API_URL", "http://localhost:8081")
	t.Setenv("WHISPER_GRPC_HOST", "myhost")
	t.Setenv("WHISPER_GRPC_PORT", "1234")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BotToken != "token123" {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, "token123")
	}
	if cfg.RootID != 42 {
		t.Errorf("RootID = %d, want 42", cfg.RootID)
	}
	if cfg.LocalAPIURL != "http://localhost:8081" {
		t.Errorf("LocalAPIURL = %q", cfg.LocalAPIURL)
	}
	if cfg.WhisperHost != "myhost" {
		t.Errorf("WhisperHost = %q, want myhost", cfg.WhisperHost)
	}
	if cfg.WhisperPort != "1234" {
		t.Errorf("WhisperPort = %q, want 1234", cfg.WhisperPort)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("BOT_TOKEN", "tok")
	t.Setenv("ROOT_ID", "7")
	t.Setenv("TELEGRAM_LOCAL_API_URL", "")
	t.Setenv("WHISPER_GRPC_HOST", "")
	t.Setenv("WHISPER_GRPC_PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WhisperHost != "localhost" {
		t.Errorf("default WhisperHost = %q, want localhost", cfg.WhisperHost)
	}
	if cfg.WhisperPort != "50053" {
		t.Errorf("default WhisperPort = %q, want 50053", cfg.WhisperPort)
	}
	if cfg.LocalAPIURL != "" {
		t.Errorf("LocalAPIURL should be empty, got %q", cfg.LocalAPIURL)
	}
}

func TestLoad_MissingToken(t *testing.T) {
	t.Setenv("BOT_TOKEN", "")
	t.Setenv("ROOT_ID", "1")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing BOT_TOKEN")
	}
}

func TestLoad_MissingRootID(t *testing.T) {
	t.Setenv("BOT_TOKEN", "tok")
	t.Setenv("ROOT_ID", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing ROOT_ID")
	}
}

func TestLoad_InvalidRootID(t *testing.T) {
	t.Setenv("BOT_TOKEN", "tok")
	t.Setenv("ROOT_ID", "notanumber")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid ROOT_ID")
	}
}

func TestLoad_ZeroRootID(t *testing.T) {
	t.Setenv("BOT_TOKEN", "tok")
	t.Setenv("ROOT_ID", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for ROOT_ID=0")
	}
}
