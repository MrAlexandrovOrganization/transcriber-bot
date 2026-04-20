package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"transcriber-bot/config"
	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/whisper"
)

const (
	pollInterval = 300 * time.Millisecond
	pollDeadline = 3 * time.Hour
	maxMsgRunes  = 4096 - 128 // Telegram message length limit
)

// All clocks: 🕐🕜🕑🕝🕒🕞🕓🕟🕔🕠🕕🕡🕖🕢🕗🕣🕘🕤🕙🕥🕚🕦🕛🕧
// var clocks = []string{
// 	"🕐", "🕑", "🕒", "🕓", "🕔", "🕕", "🕖", "🕗", "🕘", "🕙", "🕚", "🕛",
// }

type Bot struct {
	api        *tgbotapi.BotAPI
	cfg        *config.Config
	client     *whisper.Client
	cancels    sync.Map // jobID → context.CancelFunc
	userPreset sync.Map // userID → preset name (string)
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
		if update.CallbackQuery != nil {
			go b.handleCallback(update.CallbackQuery)
		} else {
			go b.handle(update)
		}
	}
}

func cancelKeyboard(jobID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отменить", "cancel:"+jobID),
		),
	)
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
		b.handleCommand(msg)
		return
	}

	fileID, format := extractFile(msg)
	if fileID == "" {
		return
	}
	slog.Info("file received", "file_id", fileID, "format", format)

	// Determine effective preset.
	storedPreset := ""
	if v, ok := b.userPreset.Load(msg.From.ID); ok {
		storedPreset = v.(string)
	}
	effectivePreset := resolvePreset(storedPreset, msg)
	slog.Info("using preset", "preset", effectivePreset)

	rc, err := b.downloadFile(fileID)
	if err != nil {
		slog.Error("download file", "error", err)
		b.replyTo(msg, "Не удалось скачать файл.")
		return
	}
	defer rc.Close()

	opts := buildOptions(effectivePreset)
	jobID, queuePos, err := b.client.Submit(rc, format, opts)
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

	slog.Info("job submitted", "job_id", jobID, "queue_pos", queuePos, "preset", effectivePreset)

	statusText := "⏳ Расшифровываю..."
	if queuePos > 1 {
		statusText = fmt.Sprintf("⏳ В очереди (позиция %d), подожди немного...", queuePos)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancels.Store(jobID, cancel)

	m := tgbotapi.NewMessage(msg.Chat.ID, statusText)
	m.ReplyToMessageID = msg.MessageID
	m.ReplyMarkup = cancelKeyboard(jobID)
	statusMsg, err := b.api.Send(m)
	if err != nil {
		slog.Error("send status message", "error", err)
		cancel()
		b.cancels.Delete(jobID)
		return
	}
	slog.Info("replied", "chat_id", msg.Chat.ID, "msg_id", statusMsg.MessageID)

	go b.pollAndUpdate(ctx, cancel, msg, statusMsg.MessageID, jobID, effectivePreset)
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		b.replyTo(msg, "Привет! Отправь мне голосовое сообщение, кружочек или видео — я расшифрую их в текст.\n\nИспользуй /preset для выбора режима расшифровки.")
	case "preset":
		b.sendPresetKeyboard(msg)
	}
}

func (b *Bot) sendPresetKeyboard(msg *tgbotapi.Message) {
	currentPreset := "auto"
	if v, ok := b.userPreset.Load(msg.From.ID); ok {
		currentPreset = v.(string)
	}

	// Build keyboard: 2 buttons per row.
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(availablePresets); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < len(availablePresets); j++ {
			p := availablePresets[j]
			label := p.label
			if p.name == currentPreset {
				label += " ✓"
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "preset:"+p.name))
		}
		rows = append(rows, row)
	}

	currentLabel := presetLabels[currentPreset]
	m := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("Выбери режим расшифровки.\nТекущий: %s", currentLabel))
	m.ReplyToMessageID = msg.MessageID
	m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	if _, err := b.api.Send(m); err != nil {
		slog.Error("send preset keyboard", "error", err)
	}
}

// pollAndUpdate periodically polls the job status and edits the status message when done.
func (b *Bot) pollAndUpdate(ctx context.Context, cancel context.CancelFunc, origMsg *tgbotapi.Message, statusMsgID int, jobID, preset string) {
	defer func() {
		cancel()
		b.cancels.Delete(jobID)
	}()

	slog.Info("polling started", "job_id", jobID, "status_msg_id", statusMsgID)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(pollDeadline)
	// tick := 0

	keyboard := cancelKeyboard(jobID)

	for {
		select {
		case <-ctx.Done():
			slog.Info("job cancelled", "job_id", jobID)
			b.editFinal(origMsg.Chat.ID, statusMsgID, "❌ Отменено.")
			return
		case <-deadline:
			slog.Warn("poll deadline exceeded", "job_id", jobID)
			b.editFinal(origMsg.Chat.ID, statusMsgID, "Превышено время ожидания (3 часа). Попробуй ещё раз.")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			result, err := b.client.GetStatus(jobID)
			if err != nil {
				slog.Warn("poll status", "job_id", jobID, "error", err)
				continue
			}
			slog.Info("poll status", "job_id", jobID, "status", result.Status)
			// if tick%10 == 0 {
			// 	slog.Info("poll status", "job_id", jobID, "status", result.Status)
			// }
			// clock := clocks[tick%len(clocks)]
			// tick++
			switch result.Status {
			case pb.JobStatus_PENDING:
				b.edit(origMsg.Chat.ID, statusMsgID /*clock+*/, " В очереди...", keyboard)
			case pb.JobStatus_RUNNING:
				b.edit(origMsg.Chat.ID, statusMsgID /*clock+*/, " Расшифровываю...", keyboard)
			case pb.JobStatus_DONE:
				text := formatResult(result, preset)
				if text == "" {
					text = "(тишина)"
				}
				slog.Info("job done", "job_id", jobID, "text_runes", len([]rune(text)))
				if preset == "lecture" {
					b.editFinal(origMsg.Chat.ID, statusMsgID, "✅ Готово!")
					b.sendAsFile(origMsg, text, origMsg.MessageID)
				} else {
					parts := splitText(text)
					b.editFinal(origMsg.Chat.ID, statusMsgID, parts[0])
					for _, part := range parts[1:] {
						b.replyTo(origMsg, part)
					}
				}
				return
			case pb.JobStatus_FAILED:
				if result.Error == "cancelled" {
					return // already handled via ctx.Done()
				}
				slog.Error("job failed", "job_id", jobID, "error", result.Error)
				b.editFinal(origMsg.Chat.ID, statusMsgID, "Произошла ошибка при расшифровке.")
				return
			}
		}
	}
}

// formatResult formats the transcription result for display.
// For lecture preset (with segments), formats with timestamps.
// For other presets, returns plain text.
func formatResult(result *whisper.JobResult, preset string) string {
	if preset == "lecture" && len(result.Segments) > 0 {
		var sb strings.Builder
		for _, seg := range result.Segments {
			fmt.Fprintf(&sb, "[%.1fs → %.1fs] %s\n", seg.Start, seg.End, seg.Text)
		}
		return strings.TrimSpace(sb.String())
	}
	return result.Text
}

// handleCallback processes inline keyboard button presses.
func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	if _, err := b.api.Request(tgbotapi.NewCallback(cb.ID, "")); err != nil {
		slog.Warn("answer callback", "error", err)
	}

	switch {
	case strings.HasPrefix(cb.Data, "cancel:"):
		b.handleCancelCallback(cb)
	case strings.HasPrefix(cb.Data, "preset:"):
		b.handlePresetCallback(cb)
	}
}

func (b *Bot) handleCancelCallback(cb *tgbotapi.CallbackQuery) {
	jobID := strings.TrimPrefix(cb.Data, "cancel:")
	slog.Info("cancel requested", "job_id", jobID)

	val, ok := b.cancels.LoadAndDelete(jobID)
	if !ok {
		slog.Warn("cancel: job not found or already finished", "job_id", jobID)
		return
	}
	val.(context.CancelFunc)()

	if cancelled, err := b.client.Cancel(jobID); err != nil {
		slog.Warn("cancel job on backend", "job_id", jobID, "error", err)
	} else {
		slog.Info("cancel sent to backend", "job_id", jobID, "cancelled", cancelled)
	}
}

func (b *Bot) handlePresetCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.From == nil {
		return
	}
	presetName := strings.TrimPrefix(cb.Data, "preset:")

	// Validate preset name.
	valid := false
	for _, p := range availablePresets {
		if p.name == presetName {
			valid = true
			break
		}
	}
	if !valid {
		slog.Warn("unknown preset", "preset", presetName)
		return
	}

	b.userPreset.Store(cb.From.ID, presetName)
	slog.Info("preset selected", "user_id", cb.From.ID, "preset", presetName)

	label := presetLabels[presetName]

	// Update the keyboard to show the new selection.
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(availablePresets); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < len(availablePresets); j++ {
			p := availablePresets[j]
			btnLabel := p.label
			if p.name == presetName {
				btnLabel += " ✓"
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(btnLabel, "preset:"+p.name))
		}
		rows = append(rows, row)
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(
		cb.Message.Chat.ID,
		cb.Message.MessageID,
		fmt.Sprintf("Выбери режим расшифровки.\nТекущий: %s", label),
		tgbotapi.NewInlineKeyboardMarkup(rows...),
	)
	if _, err := b.api.Send(edit); err != nil {
		slog.Warn("update preset keyboard", "error", err)
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

func (b *Bot) sendAsFile(orig *tgbotapi.Message, text string, replyToID int) {
	fileName := fmt.Sprintf("lecture_%d.txt", orig.MessageID)
	doc := tgbotapi.NewDocument(orig.Chat.ID, tgbotapi.FileBytes{
		Name:  fileName,
		Bytes: []byte(text),
	})
	doc.ReplyToMessageID = replyToID
	if _, err := b.api.Send(doc); err != nil {
		slog.Error("send file failed", "chat_id", orig.Chat.ID, "error", err)
	} else {
		slog.Info("sent as file", "chat_id", orig.Chat.ID, "file", fileName)
	}
}

func (b *Bot) replyTo(orig *tgbotapi.Message, text string) tgbotapi.Message {
	m := tgbotapi.NewMessage(orig.Chat.ID, text)
	m.ReplyToMessageID = orig.MessageID
	sent, err := b.api.Send(m)
	if err != nil {
		slog.Error("reply failed", "chat_id", orig.Chat.ID, "text_runes", len([]rune(text)), "error", err)
	} else {
		slog.Info("replied", "chat_id", orig.Chat.ID, "msg_id", sent.MessageID)
	}
	return sent
}

func (b *Bot) edit(chatID int64, msgID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	textRunes := len([]rune(text))
	slog.Info("editing message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := tgbotapi.NewEditMessageText(chatID, msgID, text)
	cfg.ReplyMarkup = &keyboard
	if _, err := b.api.Send(cfg); err != nil {
		slog.Error("edit message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
}

// editFinal edits the message text and removes the inline keyboard.
func (b *Bot) editFinal(chatID int64, msgID int, text string) {
	textRunes := len([]rune(text))
	slog.Info("editing final message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := tgbotapi.NewEditMessageText(chatID, msgID, text)
	if _, err := b.api.Send(cfg); err != nil {
		slog.Error("edit final message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
}

// splitText splits text into chunks that fit within Telegram's message length limit.
func splitText(text string) []string {
	runes := []rune(text)
	if len(runes) <= maxMsgRunes {
		return []string{text}
	}
	var parts []string
	for len(runes) > 0 {
		end := min(maxMsgRunes, len(runes))
		parts = append(parts, string(runes[:end]))
		runes = runes[end:]
	}
	return parts
}
