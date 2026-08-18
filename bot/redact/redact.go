// Package redact wraps a slog.Handler to mask secrets (e.g. the bot token)
// from every logged record, including inside error messages and URLs.
package redact

import (
	"context"
	"log/slog"
	"strings"
)

const masked = "***"

// Handler is a slog.Handler that masks occurrences of secrets in all
// logged messages, attribute values and nested groups.
type Handler struct {
	slog.Handler
	secrets []string
}

// New wraps h so that any occurrence of secrets in log records is replaced
// with "***". If no non-empty secrets are given, h is returned unchanged.
func New(h slog.Handler, secrets ...string) slog.Handler {
	secrets = nonEmpty(secrets)
	if len(secrets) == 0 {
		return h
	}
	return &Handler{Handler: h, secrets: secrets}
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	redacted := slog.NewRecord(r.Time, r.Level, mask(r.Message, h.secrets), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		redacted.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.Handler.Handle(ctx, redacted)
}

func (h *Handler) redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		children := a.Value.Group()
		for i := range children {
			children[i] = h.redactAttr(children[i])
		}
		a.Value = slog.GroupValue(children...)
		return a
	}
	a.Value = slog.StringValue(mask(a.Value.String(), h.secrets))
	return a
}

func mask(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, masked)
	}
	return s
}

func nonEmpty(secrets []string) []string {
	var out []string
	for _, s := range secrets {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
