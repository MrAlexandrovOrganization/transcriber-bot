package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestExtractFile_Voice(t *testing.T) {
	msg := &tgbotapi.Message{
		Voice: &tgbotapi.Voice{FileID: "voice-123"},
	}
	fileID, format := extractFile(msg)
	if fileID != "voice-123" {
		t.Errorf("fileID = %q, want voice-123", fileID)
	}
	if format != "ogg" {
		t.Errorf("format = %q, want ogg", format)
	}
}

func TestExtractFile_VideoNote(t *testing.T) {
	msg := &tgbotapi.Message{
		VideoNote: &tgbotapi.VideoNote{FileID: "vnote-456"},
	}
	fileID, format := extractFile(msg)
	if fileID != "vnote-456" {
		t.Errorf("fileID = %q, want vnote-456", fileID)
	}
	if format != "mp4" {
		t.Errorf("format = %q, want mp4", format)
	}
}

func TestExtractFile_Video(t *testing.T) {
	msg := &tgbotapi.Message{
		Video: &tgbotapi.Video{FileID: "vid-789"},
	}
	fileID, format := extractFile(msg)
	if fileID != "vid-789" {
		t.Errorf("fileID = %q, want vid-789", fileID)
	}
	if format != "mp4" {
		t.Errorf("format = %q, want mp4", format)
	}
}

func TestExtractFile_Document(t *testing.T) {
	tests := []struct {
		name       string
		fileName   string
		wantFormat string
	}{
		{"mp4 extension", "lecture.mp4", "mp4"},
		{"avi extension", "clip.AVI", "avi"},
		{"no extension", "audiofile", "mp4"},
		{"ogg extension", "voice.ogg", "ogg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &tgbotapi.Message{
				Document: &tgbotapi.Document{
					FileID:   "doc-id",
					FileName: tt.fileName,
				},
			}
			fileID, format := extractFile(msg)
			if fileID != "doc-id" {
				t.Errorf("fileID = %q, want doc-id", fileID)
			}
			if format != tt.wantFormat {
				t.Errorf("format = %q, want %q", format, tt.wantFormat)
			}
		})
	}
}

func TestExtractFile_NoMedia(t *testing.T) {
	msg := &tgbotapi.Message{
		Text: "just a text message",
	}
	fileID, format := extractFile(msg)
	if fileID != "" || format != "" {
		t.Errorf("expected empty result for text message, got fileID=%q format=%q", fileID, format)
	}
}
