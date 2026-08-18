package redact

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func capture(secrets []string) (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	h := New(slog.NewTextHandler(&buf, nil), secrets...)
	return &buf, slog.New(h)
}

func TestNew_NoSecrets(t *testing.T) {
	h := New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if _, ok := h.(*Handler); ok {
		t.Fatal("expected the base handler to be returned unchanged when no secrets are given")
	}
}

func TestMask_StringAttr(t *testing.T) {
	buf, log := capture([]string{"token123"})
	log.Info("started", "url", "https://api.telegram.org/file/bottoken123/foo.mp4")

	if got := buf.String(); strings.Contains(got, "token123") {
		t.Fatalf("secret leaked in output: %q", got)
	} else if !strings.Contains(got, "bot***") {
		t.Fatalf("expected masked value in output, got: %q", got)
	}
}

func TestMask_ErrorMessage(t *testing.T) {
	buf, log := capture([]string{"token123"})
	log.Error("download failed", "error", &urlError{url: "https://api.telegram.org/file/bottoken123/foo.mp4"})

	if got := buf.String(); strings.Contains(got, "token123") {
		t.Fatalf("secret leaked in error attr: %q", got)
	} else if !strings.Contains(got, "bot***") {
		t.Fatalf("expected masked error in output, got: %q", got)
	}
}

func TestMask_Message(t *testing.T) {
	buf, log := capture([]string{"token123"})
	log.Info("authenticating with token123")

	if got := buf.String(); strings.Contains(got, "token123") {
		t.Fatalf("secret leaked in message: %q", got)
	}
}

func TestMask_Group(t *testing.T) {
	buf, log := capture([]string{"token123"})
	log.Info("update", "request", slog.Group("file",
		slog.String("url", "https://api.telegram.org/file/bottoken123/foo.mp4"),
		slog.Int("size", 10),
	))

	got := buf.String()
	if strings.Contains(got, "token123") {
		t.Fatalf("secret leaked in group: %q", got)
	}
	if !strings.Contains(got, "bot***") {
		t.Fatalf("expected masked group value, got: %q", got)
	}
	if !strings.Contains(got, "size=10") {
		t.Fatalf("group structure should be preserved, got: %q", got)
	}
}

func TestMask_UnrelatedValueUntouched(t *testing.T) {
	buf, log := capture([]string{"token123"})
	log.Info("update", "user_id", 42, "job_id", "job-abc")

	got := buf.String()
	if !strings.Contains(got, "user_id=42") || !strings.Contains(got, "job_id=job-abc") {
		t.Fatalf("unrelated values should be untouched, got: %q", got)
	}
}

type urlError struct{ url string }

func (e *urlError) Error() string { return "Get \"" + e.url + "\": some error" }
