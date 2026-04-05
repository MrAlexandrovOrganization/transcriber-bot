package bot

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"transcriber-bot/config"
	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/whisper"
)

const (
	pollInterval = 5 * time.Second
	pollDeadline = 3 * time.Hour
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
			b.replyTo(msg, "Привет! Пересылай мне голосовые сообщения, кружочки или видео — я расшифрую их в текст.")
		}
		return
	}

	fileID, format := extractFile(msg)
	if fileID == "" {
		return
	}

	rc, err := b.downloadFile(fileID)
	if err != nil {
		slog.Error("download file", "error", err)
		b.replyTo(msg, "Не удалось скачать файл.")
		return
	}
	defer rc.Close()

	jobID, queuePos, err := b.client.Submit(rc, format)
	if err != nil {
		var unavail *whisper.UnavailableError
		if errors.As(err, &unavail) {
			b.replyTo(msg, "Сервис транскрипции недоступен, попробуй позже.")
		} else {
			slog.Error("submit job", "error", err)
			b.replyTo(msg, "Произошла ошибка при отправке на расшифровку.")
		}
		return
	}

	statusText := "⏳ Расшифровываю..."
	if queuePos > 1 {
		statusText = fmt.Sprintf("⏳ В очереди (позиция %d), подожди немного...", queuePos)
	}
	statusMsg := b.replyTo(msg, statusText)

	go b.pollAndUpdate(msg, statusMsg.MessageID, jobID)
}

// pollAndUpdate periodically polls the job status and edits the status message when done.
func (b *Bot) pollAndUpdate(origMsg *tgbotapi.Message, statusMsgID int, jobID string) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(pollDeadline)

	for {
		select {
		case <-deadline:
			b.edit(origMsg.Chat.ID, statusMsgID, "Превышено время ожидания (3 часа). Попробуй ещё раз.")
			return
		case <-ticker.C:
			result, err := b.client.GetStatus(jobID)
			if err != nil {
				slog.Warn("poll status", "job_id", jobID, "error", err)
				continue
			}
			switch result.Status {
			case pb.JobStatus_DONE:
				text := result.Text
				if text == "" {
					text = "(тишина)"
				}
				b.edit(origMsg.Chat.ID, statusMsgID, text)
				return
			case pb.JobStatus_FAILED:
				slog.Error("job failed", "job_id", jobID, "error", result.Error)
				b.edit(origMsg.Chat.ID, statusMsgID, "Произошла ошибка при расшифровке.")
				return
			}
		}
	}
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

func (b *Bot) downloadFile(fileID string) (io.ReadCloser, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}

	if b.cfg.LocalAPIURL != "" {
		slog.Info("reading local file", "path", file.FilePath)
		f, err := os.Open(file.FilePath)
		if err != nil {
			return nil, fmt.Errorf("open local file: %w", err)
		}
		return f, nil
	}

	fileURL := file.Link(b.cfg.BotToken)
	slog.Info("downloading file", "url", fileURL)
	resp, err := http.Get(fileURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: status %d, body: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

func (b *Bot) replyTo(orig *tgbotapi.Message, text string) tgbotapi.Message {
	m := tgbotapi.NewMessage(orig.Chat.ID, text)
	m.ReplyToMessageID = orig.MessageID
	sent, err := b.api.Send(m)
	if err != nil {
		slog.Error("reply", "error", err)
	}
	return sent
}

func (b *Bot) edit(chatID int64, msgID int, text string) {
	if _, err := b.api.Send(tgbotapi.NewEditMessageText(chatID, msgID, text)); err != nil {
		slog.Error("edit message", "error", err)
	}
}
