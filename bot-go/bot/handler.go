package bot

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"transcriber-bot/config"
	"transcriber-bot/whisper"
)

type Bot struct {
	api    *tgbotapi.BotAPI
	cfg    *config.Config
	client *whisper.Client
}

func New(cfg *config.Config, wc *whisper.Client) (*Bot, error) {
	var (
		api *tgbotapi.BotAPI
		err error
	)
	if cfg.LocalAPIURL != "" {
		api, err = tgbotapi.NewBotAPIWithAPIEndpoint(cfg.BotToken, cfg.LocalAPIURL+"/bot%s/%s")
	} else {
		api, err = tgbotapi.NewBotAPI(cfg.BotToken)
	}
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	slog.Info("authorized", "username", api.Self.UserName)
	return &Bot{api: api, cfg: cfg, client: wc}, nil
}

func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		go b.handle(update)
	}
}

func (b *Bot) handle(update tgbotapi.Update) {
	msg := update.Message
	if msg == nil || msg.From == nil {
		return
	}

	slog.Info("incoming",
		"update_id", update.UpdateID,
		"user_id", msg.From.ID,
		"voice", msg.Voice != nil,
		"video_note", msg.VideoNote != nil,
		"video", msg.Video != nil,
		"document", msg.Document != nil,
	)

	if msg.From.ID != b.cfg.RootID {
		slog.Warn("unauthorized", "user_id", msg.From.ID)
		return
	}

	if msg.IsCommand() {
		if msg.Command() == "start" {
			b.reply(msg, "Привет! Пересылай мне голосовые сообщения, кружочки или видео — я расшифрую их в текст.")
		}
		return
	}

	fileID, format := extractFile(msg)
	if fileID == "" {
		return
	}

	statusMsg, err := b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "⏳ Транскрибирую..."))
	if err != nil {
		slog.Error("send status", "error", err)
		return
	}

	text, err := b.transcribe(fileID, format)
	if err != nil {
		var unavail *whisper.UnavailableError
		if errors.As(err, &unavail) {
			b.edit(msg.Chat.ID, statusMsg.MessageID, "Сервис транскрипции недоступен, попробуй позже.")
		} else {
			slog.Error("transcription failed", "error", err)
			b.edit(msg.Chat.ID, statusMsg.MessageID, "Произошла ошибка при транскрипции.")
		}
		return
	}

	if text == "" {
		text = "(тишина)"
	}
	b.edit(msg.Chat.ID, statusMsg.MessageID, text)
}

func extractFile(msg *tgbotapi.Message) (fileID, format string) {
	switch {
	case msg.Voice != nil:
		return msg.Voice.FileID, "ogg"
	case msg.VideoNote != nil:
		return msg.VideoNote.FileID, "mp4"
	case msg.Video != nil:
		return msg.Video.FileID, "mp4"
	case msg.Document != nil:
		ext := "mp4"
		if i := strings.LastIndex(msg.Document.FileName, "."); i >= 0 {
			ext = strings.ToLower(msg.Document.FileName[i+1:])
		}
		return msg.Document.FileID, ext
	}
	return "", ""
}

func (b *Bot) transcribe(fileID, format string) (string, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get file info: %w", err)
	}

	var data []byte
	if b.cfg.LocalAPIURL != "" {
		// In local mode file_path is an absolute path on the shared volume — read directly.
		slog.Info("reading local file", "path", file.FilePath)
		data, err = os.ReadFile(file.FilePath)
		if err != nil {
			return "", fmt.Errorf("read local file: %w", err)
		}
	} else {
		fileURL := file.Link(b.cfg.BotToken)
		slog.Info("downloading file", "url", fileURL)
		resp, err := http.Get(fileURL) //nolint:noctx
		if err != nil {
			return "", fmt.Errorf("download: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("download failed: status %d, body: %s", resp.StatusCode, body)
		}
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("read body: %w", err)
		}
	}

	slog.Info("got audio", "bytes", len(data), "format", format)
	return b.client.Transcribe(data, format)
}

func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	if _, err := b.api.Send(tgbotapi.NewMessage(msg.Chat.ID, text)); err != nil {
		slog.Error("reply", "error", err)
	}
}

func (b *Bot) edit(chatID int64, msgID int, text string) {
	if _, err := b.api.Send(tgbotapi.NewEditMessageText(chatID, msgID, text)); err != nil {
		slog.Error("edit message", "error", err)
	}
}
