package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"transcriber-bot/config"
	"transcriber-bot/resources"
	"transcriber-bot/store"
	"transcriber-bot/tgfmt"
	"transcriber-bot/whisper"
)

const (
	pollInterval         = 5 * time.Second
	maxMsgRunes          = 4096 - 128 // Telegram message length limit
	downloadTimeout      = 30 * time.Minute
	maxDownloadErrorBody = 4 * 1024
)

type Bot struct {
	api        *telego.Bot
	cfg        *config.Config
	client     *whisper.Client
	chats      *store.Store
	userPreset sync.Map // userID → preset name (string)

	botID int64
}

func New(cfg *config.Config, wc *whisper.Client, chats *store.Store) (*Bot, error) {
	options := []telego.BotOption{
		telego.WithHTTPClient(&http.Client{Timeout: 70 * time.Second}),
		// All errors are logged by the application through its token-masking logger.
		telego.WithDiscardLogger(),
	}
	if cfg.LocalAPIURL != "" {
		options = append(options, telego.WithAPIServer(strings.TrimRight(cfg.LocalAPIURL, "/")))
	}
	api, err := telego.NewBot(cfg.BotToken, options...)
	if err != nil {
		return nil, fmt.Errorf("init bot api: %w", err)
	}
	self, err := api.GetMe(context.Background())
	if err != nil {
		return nil, fmt.Errorf("authorize bot api: %w", err)
	}
	slog.Info(
		"authorized",
		"username", self.Username,
		"local_api_url", cfg.LocalAPIURL,
		"root_id", cfg.RootID,
	)
	return &Bot{api: api, cfg: cfg, client: wc, chats: chats, botID: self.ID}, nil
}

func maskBotToken(err error, token string) string {
	if err == nil {
		return ""
	}
	if token == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), token, "***")
}

func cancelKeyboard(jobID string) *telego.InlineKeyboardMarkup {
	return tu.InlineKeyboard(tu.InlineKeyboardRow(tu.InlineKeyboardButton(resources.CancelButton).WithCallbackData("cancel:" + jobID)))
}

// isGroupChat reports whether the chat is a group or supergroup.
// Private chats are authorized via ROOT_ID and are never stored.
func isGroupChat(chat telego.Chat) bool {
	return chat.Type == "group" || chat.Type == "supergroup"
}

// Telegram marks commands with entities; plain text beginning with '/' is not sufficient.
func messageCommand(msg *telego.Message) string {
	if len(msg.Entities) == 0 || msg.Entities[0].Type != "bot_command" || msg.Entities[0].Offset != 0 {
		return ""
	}
	command, _, _ := tu.ParseCommand(msg.Text)
	return command
}

func (b *Bot) handleCommand(ctx context.Context, msg *telego.Message) {
	switch messageCommand(msg) {
	case "start":
		slog.InfoContext(ctx, "handling command", "command", "start", "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
		b.replyTo(ctx, msg, "Привет! Отправь мне голосовое сообщение, кружочек или видео — я расшифрую их в текст.\n\nИспользуй /preset для выбора режима расшифровки.")
	case "preset":
		slog.InfoContext(ctx, "handling command", "command", "preset", "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
		b.sendPresetKeyboard(ctx, msg)
	default:
		slog.InfoContext(ctx, "unknown command", "command", messageCommand(msg), "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
	}
}

func (b *Bot) sendPresetKeyboard(ctx context.Context, msg *telego.Message) {
	currentPreset := "auto"
	if v, ok := b.userPreset.Load(msg.From.ID); ok {
		currentPreset = v.(string)
	}

	// Build keyboard: 2 buttons per row.
	var rows [][]telego.InlineKeyboardButton
	for i := 0; i < len(availablePresets); i += 2 {
		var row []telego.InlineKeyboardButton
		for j := i; j < i+2 && j < len(availablePresets); j++ {
			p := availablePresets[j]
			label := p.label
			if p.name == currentPreset {
				label += " ✓"
			}
			row = append(row, tu.InlineKeyboardButton(label).WithCallbackData("preset:"+p.name))
		}
		rows = append(rows, row)
	}

	currentLabel := presetLabels[currentPreset]
	m := messageParams(msg.Chat.ID, tgfmt.Escape(fmt.Sprintf("Выбери режим расшифровки.\nТекущий: %s", currentLabel)))
	m.ReplyParameters = &telego.ReplyParameters{MessageID: msg.MessageID, ChatID: tu.ID(msg.Chat.ID), AllowSendingWithoutReply: true}
	m.ReplyMarkup = tu.InlineKeyboard(rows...)
	if _, err := b.api.SendMessage(ctx, m); err != nil {
		slog.Error("send preset keyboard", "error", err)
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

// quoteText renders a transcription result as a Telegram block quote.
func quoteText(text string) tgfmt.HTML {
	return tgfmt.Blockquote(tgfmt.Escape(text))
}

// answerCallback acknowledges a callback query, optionally showing text
// as a popup to the user who pressed the button.
func (b *Bot) answerCallback(ctx context.Context, id, text string) {
	if err := b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text}); err != nil {
		slog.Warn("answer callback", "error", err)
	}
}

func (b *Bot) handlePresetCallback(ctx context.Context, cb *telego.CallbackQuery) {
	if cb.Message == nil {
		return
	}
	b.answerCallback(ctx, cb.ID, "")
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
	var rows [][]telego.InlineKeyboardButton
	for i := 0; i < len(availablePresets); i += 2 {
		var row []telego.InlineKeyboardButton
		for j := i; j < i+2 && j < len(availablePresets); j++ {
			p := availablePresets[j]
			btnLabel := p.label
			if p.name == presetName {
				btnLabel += " ✓"
			}
			row = append(row, tu.InlineKeyboardButton(btnLabel).WithCallbackData("preset:"+p.name))
		}
		rows = append(rows, row)
	}

	edit := editParams(cb.Message.GetChat().ID, cb.Message.GetMessageID(), tgfmt.Escape(fmt.Sprintf("Выбери режим расшифровки.\nТекущий: %s", label)))
	edit.ReplyMarkup = tu.InlineKeyboard(rows...)
	if _, err := b.api.EditMessageText(ctx, edit); err != nil {
		slog.Warn("update preset keyboard", "error", err)
	}
}

func extractFile(msg *telego.Message) (fileID, format string) {
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

func (b *Bot) downloadFile(ctx context.Context, fileID string, onProgress func(downloaded, total int64)) (io.ReadCloser, error) {
	slog.InfoContext(ctx, "requesting file info")
	file, err := b.api.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}
	slog.InfoContext(ctx, "file info received", "file_size", file.FileSize)

	if b.cfg.LocalAPIURL != "" {
		slog.InfoContext(ctx, "reading local file", "path", file.FilePath)
		f, err := os.Open(file.FilePath)
		if err != nil {
			return nil, fmt.Errorf("open local file: %w", err)
		}
		if onProgress != nil {
			if stat, statErr := f.Stat(); statErr == nil {
				onProgress(stat.Size(), stat.Size())
			}
		}
		return f, nil
	}

	fileURL := b.api.FileDownloadURL(file.FilePath)
	slog.InfoContext(ctx, "downloading file")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDownloadErrorBody))
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: status %d, body: %s", resp.StatusCode, body)
	}
	return newProgressReadCloser(resp.Body, resp.ContentLength, onProgress), nil
}

func mediaNoun(msg *telego.Message) string {
	switch {
	case msg.Voice != nil:
		return "голосовое"
	case msg.VideoNote != nil:
		return "кружочек"
	case msg.Video != nil:
		return "видео"
	case msg.Document != nil:
		return "документ"
	}
	return "файл"
}

func (b *Bot) sendInitialStatus(ctx context.Context, msg *telego.Message, noun string) (*telego.Message, error) {
	m := messageParams(msg.Chat.ID, tgfmt.Escape(fmt.Sprintf(resources.QueuedMedia, noun)))
	m.ReplyParameters = &telego.ReplyParameters{MessageID: msg.MessageID, ChatID: tu.ID(msg.Chat.ID), AllowSendingWithoutReply: true}

	statusMsg, err := b.api.SendMessage(ctx, m)
	if err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "sent initial status", "chat_id", msg.Chat.ID, "msg_id", statusMsg.MessageID)
	return statusMsg, nil
}

func (b *Bot) replyTo(ctx context.Context, orig *telego.Message, text string) *telego.Message {
	return b.replyToHTML(ctx, orig, tgfmt.Escape(text))
}

func (b *Bot) replyToHTML(ctx context.Context, orig *telego.Message, text tgfmt.HTML) *telego.Message {
	m := messageParams(orig.Chat.ID, text)
	m.ReplyParameters = &telego.ReplyParameters{MessageID: orig.MessageID, ChatID: tu.ID(orig.Chat.ID)}
	sent, err := b.api.SendMessage(ctx, m)
	if err != nil {
		slog.Error("reply failed", "chat_id", orig.Chat.ID, "text_runes", len([]rune(text)), "error", err)
	} else {
		slog.Info("replied", "chat_id", orig.Chat.ID, "msg_id", sent.MessageID)
	}
	return sent
}

func (b *Bot) edit(chatID int64, msgID int, text string, keyboard *telego.InlineKeyboardMarkup) {
	textRunes := len([]rune(text))
	slog.Info("editing message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := editParams(chatID, msgID, tgfmt.Escape(text))
	cfg.ReplyMarkup = keyboard
	if _, err := b.api.EditMessageText(context.Background(), cfg); err != nil {
		if isMessageNotModifiedError(err) {
			return
		}
		slog.Error("edit message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
}

// editFinal edits the message text and removes the inline keyboard.
func (b *Bot) editFinal(chatID int64, msgID int, text string) {
	b.editFinalHTML(chatID, msgID, tgfmt.Escape(text))
}

func (b *Bot) editFinalHTML(chatID int64, msgID int, text tgfmt.HTML) {
	textRunes := len([]rune(text))
	slog.Info("editing final message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := editParams(chatID, msgID, text)
	if _, err := b.api.EditMessageText(context.Background(), cfg); err != nil {
		if isMessageNotModifiedError(err) {
			return
		}
		slog.Error("edit final message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
}

func messageParams(chatID int64, text tgfmt.HTML) *telego.SendMessageParams {
	return &telego.SendMessageParams{ChatID: tu.ID(chatID), Text: string(text), ParseMode: telego.ModeHTML}
}

func editParams(chatID int64, msgID int, text tgfmt.HTML) *telego.EditMessageTextParams {
	return &telego.EditMessageTextParams{ChatID: tu.ID(chatID), MessageID: msgID, Text: string(text), ParseMode: telego.ModeHTML}
}

func isMessageNotModifiedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

type progressReadCloser struct {
	reader       io.ReadCloser
	total        int64
	onProgress   func(downloaded, total int64)
	downloaded   int64
	lastReported int64
}

func newProgressReadCloser(reader io.ReadCloser, total int64, onProgress func(downloaded, total int64)) io.ReadCloser {
	return &progressReadCloser{
		reader:     reader,
		total:      total,
		onProgress: onProgress,
	}
}

func (p *progressReadCloser) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	if n > 0 {
		p.downloaded += int64(n)
		if p.onProgress != nil && (p.total <= 0 || p.downloaded-p.lastReported >= 5*1024*1024 || p.downloaded == p.total) {
			p.lastReported = p.downloaded
			p.onProgress(p.downloaded, p.total)
		}
	}
	return n, err
}

func (p *progressReadCloser) Close() error {
	return p.reader.Close()
}

func formatDownloadStatus(downloaded, total int64, noun string) string {
	if total > 0 {
		percent := float64(downloaded) / float64(total) * 100
		return fmt.Sprintf(resources.DownloadPercent, noun, percent)
	}
	return fmt.Sprintf(resources.DownloadBytes, noun, formatBytes(downloaded))
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
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
