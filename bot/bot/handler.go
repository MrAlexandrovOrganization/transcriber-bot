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
	"go.opentelemetry.io/otel"

	"transcriber-bot/config"
	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/store"
	"transcriber-bot/whisper"
)

const (
	pollInterval         = 5 * time.Second
	pollDeadline         = 3 * time.Hour
	maxMsgRunes          = 4096 - 128 // Telegram message length limit
	dedupTTL             = 1 * time.Hour
	downloadTimeout      = 30 * time.Minute
	maxConcurrentJobs    = 2
	maxDownloadErrorBody = 4 * 1024
)

type Bot struct {
	api        *tgbotapi.BotAPI
	cfg        *config.Config
	client     *whisper.Client
	chats      *store.Store
	cancels    sync.Map // jobID → context.CancelFunc
	userPreset sync.Map // userID → preset name (string)
	jobSlots   chan struct{}

	processedMsgs sync.Map // chatID:messageID → time.Time
}

func New(cfg *config.Config, wc *whisper.Client, chats *store.Store) (*Bot, error) {
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
	slog.Info(
		"authorized",
		"username", api.Self.UserName,
		"local_api_url", cfg.LocalAPIURL,
		"root_id", cfg.RootID,
	)
	return &Bot{api: api, cfg: cfg, client: wc, chats: chats, jobSlots: make(chan struct{}, maxConcurrentJobs)}, nil
}

func dedupKey(chatID int64, msgID int) string {
	return fmt.Sprintf("%d:%d", chatID, msgID)
}

func (b *Bot) markProcessed(chatID int64, msgID int) bool {
	key := dedupKey(chatID, msgID)
	if _, loaded := b.processedMsgs.LoadOrStore(key, time.Now()); loaded {
		return true // already seen
	}
	return false
}

func (b *Bot) startDedupCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-dedupTTL)
		b.processedMsgs.Range(func(key, value any) bool {
			if t, ok := value.(time.Time); ok && t.Before(cutoff) {
				b.processedMsgs.Delete(key)
			}
			return true
		})
	}
}

func (b *Bot) Run() {
	go b.startDedupCleanup()
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	slog.Info("starting updates loop", "timeout_sec", u.Timeout)
	updates := b.api.GetUpdatesChan(u)
	for update := range updates {
		ctx, span := otel.Tracer("transcriber-bot").Start(context.Background(), "telegram.update")
		slog.Info(
			"update received",
			"update_id", update.UpdateID,
			"has_message", update.Message != nil,
			"has_callback", update.CallbackQuery != nil,
			"has_my_chat_member", update.MyChatMember != nil,
		)
		switch {
		case update.CallbackQuery != nil:
			go func() { defer span.End(); b.handleCallback(ctx, update.CallbackQuery) }()
		case update.MyChatMember != nil:
			go func() { defer span.End(); b.handleMyChatMember(ctx, update.MyChatMember) }()
		default:
			go func() { defer span.End(); b.handle(ctx, update) }()
		}
	}
	slog.Warn("updates channel closed")
}

func cancelKeyboard(jobID string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отменить", "cancel:"+jobID),
		),
	)
}

// isGroupChat reports whether the chat is a group or supergroup.
// Private chats are authorized via ROOT_ID and are never stored.
func isGroupChat(chat tgbotapi.Chat) bool {
	return chat.Type == "group" || chat.Type == "supergroup"
}

// handleMyChatMember tracks chats where the bot was added or removed,
// keeping the allowlist in sync with reality.
func (b *Bot) handleMyChatMember(ctx context.Context, mcm *tgbotapi.ChatMemberUpdated) {
	if !isGroupChat(mcm.Chat) {
		slog.DebugContext(ctx, "my_chat_member ignored: not a group", "chat_id", mcm.Chat.ID, "chat_type", mcm.Chat.Type)
		return
	}

	newStatus := mcm.NewChatMember.Status
	slog.InfoContext(ctx,
		"my_chat_member",
		"chat_id", mcm.Chat.ID,
		"title", mcm.Chat.Title,
		"old_status", mcm.OldChatMember.Status,
		"new_status", newStatus,
	)

	switch newStatus {
	case "member", "administrator", "restricted":
		added, err := b.chats.Add(ctx, mcm.Chat.ID, mcm.Chat.Title, mcm.From.ID)
		if err != nil {
			slog.ErrorContext(ctx, "register chat", "chat_id", mcm.Chat.ID, "error", err)
			return
		}
		if added {
			slog.InfoContext(ctx, "chat registered", "chat_id", mcm.Chat.ID, "title", mcm.Chat.Title)
		}
	default: // left, kicked
		if err := b.chats.Remove(ctx, mcm.Chat.ID); err != nil {
			slog.ErrorContext(ctx, "unregister chat", "chat_id", mcm.Chat.ID, "error", err)
			return
		}
		slog.InfoContext(ctx, "chat unregistered", "chat_id", mcm.Chat.ID)
	}
}

// authorizeGroup ensures the group chat is in the allowlist. If the event
// about adding the bot was missed, the chat is registered lazily — but only
// when the sender is root.
func (b *Bot) authorizeGroup(ctx context.Context, msg *tgbotapi.Message) error {
	allowed, err := b.chats.Exists(ctx, msg.Chat.ID)
	if err != nil {
		slog.ErrorContext(ctx, "check chat allowed", "chat_id", msg.Chat.ID, "error", err)
		return err
	}
	if allowed {
		return nil
	}
	if msg.From.ID != b.cfg.RootID {
		slog.WarnContext(ctx, "unauthorized group", "chat_id", msg.Chat.ID, "title", msg.Chat.Title, "user_id", msg.From.ID)
		return fmt.Errorf("unauthorized group %d", msg.Chat.ID)
	}
	added, err := b.chats.Add(ctx, msg.Chat.ID, msg.Chat.Title, msg.From.ID)
	if err != nil {
		slog.ErrorContext(ctx, "register chat lazily", "chat_id", msg.Chat.ID, "error", err)
		return err
	}
	if added {
		slog.InfoContext(ctx, "chat registered lazily", "chat_id", msg.Chat.ID, "title", msg.Chat.Title)
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, update tgbotapi.Update) {
	msg := update.Message
	if msg == nil {
		slog.WarnContext(ctx, "update without message", "update_id", update.UpdateID)
		return
	}
	if msg.From == nil {
		slog.WarnContext(ctx, "message without sender", "update_id", update.UpdateID, "chat_id", msg.Chat.ID)
		return
	}

	if b.markProcessed(msg.Chat.ID, msg.MessageID) {
		slog.InfoContext(ctx, "duplicate message skipped", "chat_id", msg.Chat.ID, "message_id", msg.MessageID)
		return
	}

	isGroup := isGroupChat(*msg.Chat)

	slog.InfoContext(ctx, "incoming",
		"update_id", update.UpdateID,
		"user_id", msg.From.ID,
		"chat_id", msg.Chat.ID,
		"chat_type", msg.Chat.Type,
		"username", msg.From.UserName,
		"is_command", msg.IsCommand(),
		"voice", msg.Voice != nil,
		"video_note", msg.VideoNote != nil,
		"video", msg.Video != nil,
		"document", msg.Document != nil,
	)

	if isGroup {
		if err := b.authorizeGroup(ctx, msg); err != nil {
			return
		}
	} else if msg.From.ID != b.cfg.RootID {
		slog.WarnContext(ctx, "unauthorized private message", "user_id", msg.From.ID, "expected_root_id", b.cfg.RootID)
		return
	}

	if msg.IsCommand() {
		if isGroup && msg.From.ID != b.cfg.RootID {
			slog.InfoContext(ctx, "group command ignored: not root", "command", msg.Command(), "user_id", msg.From.ID)
			return
		}
		slog.InfoContext(ctx, "dispatching command", "command", msg.Command(), "user_id", msg.From.ID)
		b.handleCommand(ctx, msg)
		return
	}

	if isGroup && msg.Voice == nil && msg.VideoNote == nil {
		slog.InfoContext(ctx,
			"group message ignored: not voice/video_note",
			"update_id", update.UpdateID,
			"user_id", msg.From.ID,
		)
		return
	}

	fileID, format := extractFile(msg)
	if fileID == "" {
		slog.InfoContext(ctx, "message ignored: no supported media", "update_id", update.UpdateID, "user_id", msg.From.ID)
		return
	}
	slog.InfoContext(ctx, "file received", "format", format)

	// Determine effective preset.
	storedPreset := ""
	if v, ok := b.userPreset.Load(msg.From.ID); ok {
		storedPreset = v.(string)
	}
	effectivePreset := resolvePreset(storedPreset, msg)
	slog.InfoContext(ctx, "using preset", "preset", effectivePreset)

	noun := mediaNoun(msg)
	statusMsg, err := b.sendInitialStatus(ctx, msg, noun)
	if err != nil {
		slog.ErrorContext(ctx, "send initial status message", "error", err)
		return
	}

	go b.processFile(ctx, msg, statusMsg.MessageID, fileID, format, noun, effectivePreset)
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		slog.InfoContext(ctx, "handling command", "command", "start", "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
		b.replyTo(msg, "Привет! Отправь мне голосовое сообщение, кружочек или видео — я расшифрую их в текст.\n\nИспользуй /preset для выбора режима расшифровки.")
	case "preset":
		slog.InfoContext(ctx, "handling command", "command", "preset", "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
		b.sendPresetKeyboard(msg)
	default:
		slog.InfoContext(ctx, "unknown command", "command", msg.Command(), "chat_id", msg.Chat.ID, "user_id", msg.From.ID)
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

	slog.InfoContext(ctx, "polling started", "job_id", jobID, "status_msg_id", statusMsgID)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(pollDeadline)

	keyboard := cancelKeyboard(jobID)
	lastStatusText := ""

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "job cancelled", "job_id", jobID)
			b.editFinal(origMsg.Chat.ID, statusMsgID, "❌ Отменено.")
			return
		case <-deadline:
			slog.WarnContext(ctx, "poll deadline exceeded", "job_id", jobID)
			b.editFinal(origMsg.Chat.ID, statusMsgID, "Превышено время ожидания (3 часа). Попробуй ещё раз.")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			result, err := b.client.GetStatusContext(ctx, jobID)
			if err != nil {
				slog.WarnContext(ctx, "poll status", "job_id", jobID, "error", err)
				continue
			}
			slog.InfoContext(ctx, "poll status", "job_id", jobID, "status", result.Status)

			switch result.Status {
			case pb.JobStatus_ACCEPTED, pb.JobStatus_DOWNLOADING, pb.JobStatus_QUEUED:
				statusText := "⏳ В очереди..."
				if result.Status == pb.JobStatus_DOWNLOADING {
					statusText = "⏳ Файл загружен, ставлю в очередь..."
				}
				if statusText != lastStatusText {
					b.edit(origMsg.Chat.ID, statusMsgID /*clock+*/, statusText, &keyboard)
					lastStatusText = statusText
				}
			case pb.JobStatus_RUNNING:
				statusText := "⏳ Расшифровываю..."
				if result.ProgressPercent > 0 {
					statusText = fmt.Sprintf("⏳ Расшифровываю... %.0f%%", result.ProgressPercent)
				}
				if statusText != lastStatusText {
					b.edit(origMsg.Chat.ID, statusMsgID /*clock+*/, statusText, &keyboard)
					lastStatusText = statusText
				}
			case pb.JobStatus_DONE:
				text := formatResult(result, preset)
				if text == "" {
					text = "(тишина)"
				}
				slog.InfoContext(ctx, "job done", "job_id", jobID, "text_runes", len([]rune(text)))
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
				slog.ErrorContext(ctx, "job failed", "job_id", jobID, "error", result.Error)
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
func (b *Bot) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	slog.InfoContext(ctx,
		"callback received",
		"id", cb.ID,
		"data", cb.Data,
		"from_user_id", cb.From.ID,
	)
	switch {
	case strings.HasPrefix(cb.Data, "cancel:"):
		b.handleCancelCallback(ctx, cb)
	case strings.HasPrefix(cb.Data, "preset:"):
		b.handlePresetCallback(ctx, cb)
	default:
		b.answerCallback(cb.ID, "")
	}
}

// answerCallback acknowledges a callback query, optionally showing text
// as a popup to the user who pressed the button.
func (b *Bot) answerCallback(id, text string) {
	if _, err := b.api.Request(tgbotapi.NewCallback(id, text)); err != nil {
		slog.Warn("answer callback", "error", err)
	}
}

func (b *Bot) handleCancelCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	jobID := strings.TrimPrefix(cb.Data, "cancel:")

	if cb.From.ID != b.cfg.RootID {
		slog.Warn("cancel denied: not root", "job_id", jobID, "user_id", cb.From.ID)
		b.answerCallback(cb.ID, "Отменить расшифровку может только владелец бота.")
		return
	}

	b.answerCallback(cb.ID, "")
	slog.Info("cancel requested", "job_id", jobID)

	val, ok := b.cancels.LoadAndDelete(jobID)
	if !ok {
		slog.Warn("cancel: job not found or already finished", "job_id", jobID)
		return
	}
	val.(context.CancelFunc)()

	if cancelled, err := b.client.CancelContext(ctx, jobID); err != nil {
		slog.Warn("cancel job on backend", "job_id", jobID, "error", err)
	} else {
		slog.Info("cancel sent to backend", "job_id", jobID, "cancelled", cancelled)
	}
}

func (b *Bot) handlePresetCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.From == nil {
		return
	}
	b.answerCallback(cb.ID, "")
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

func (b *Bot) downloadFile(ctx context.Context, fileID string, onProgress func(downloaded, total int64)) (io.ReadCloser, error) {
	slog.InfoContext(ctx, "requesting file info")
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
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

	fileURL := file.Link(b.cfg.BotToken)
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

func mediaNoun(msg *tgbotapi.Message) string {
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

func (b *Bot) sendInitialStatus(ctx context.Context, msg *tgbotapi.Message, noun string) (tgbotapi.Message, error) {
	m := tgbotapi.NewMessage(msg.Chat.ID, "⏳ Скачиваю "+noun+"...")
	m.ReplyToMessageID = msg.MessageID

	statusMsg, err := b.api.Send(m)
	if err != nil {
		return tgbotapi.Message{}, err
	}

	slog.InfoContext(ctx, "sent initial status", "chat_id", msg.Chat.ID, "msg_id", statusMsg.MessageID)
	return statusMsg, nil
}

func (b *Bot) processFile(ctx context.Context, msg *tgbotapi.Message, statusMsgID int, fileID, format, noun, preset string) {
	select {
	case b.jobSlots <- struct{}{}:
		defer func() { <-b.jobSlots }()
	default:
		b.editFinal(msg.Chat.ID, statusMsgID, "Сейчас уже обрабатываются два файла. Попробуй ещё раз позже.")
		return
	}

	slog.InfoContext(ctx,
		"process file started",
		"chat_id", msg.Chat.ID,
		"message_id", msg.MessageID,
		"status_msg_id", statusMsgID,
		"format", format,
		"preset", preset,
	)
	lastDownloadStatus := ""
	rc, err := b.downloadFile(ctx, fileID, func(downloaded, total int64) {
		statusText := formatDownloadStatus(downloaded, total, noun)
		if statusText == lastDownloadStatus {
			return
		}
		b.edit(msg.Chat.ID, statusMsgID, statusText, nil)
		lastDownloadStatus = statusText
	})
	if err != nil {
		slog.ErrorContext(ctx, "download file", "error", err)
		b.editFinal(msg.Chat.ID, statusMsgID, "❌ Не удалось скачать "+noun+". Попробуй ещё раз позже.")
		return
	}
	defer rc.Close()

	b.edit(msg.Chat.ID, statusMsgID, "⏳ Отправляю на расшифровку...", nil)

	opts := buildOptions(preset)
	jobID, queuePos, err := b.client.SubmitContext(ctx, rc, format, opts)
	if err != nil {
		if _, ok := errors.AsType[*whisper.UnavailableError](err); ok {
			b.editFinal(msg.Chat.ID, statusMsgID, "Сервис транскрипции недоступен, попробуй позже.")
		} else {
			slog.ErrorContext(ctx, "submit job", "error", err)
			b.editFinal(msg.Chat.ID, statusMsgID, "Произошла ошибка при отправке на расшифровку.")
		}
		return
	}

	slog.InfoContext(ctx, "job submitted", "job_id", jobID, "queue_pos", queuePos, "preset", preset)

	statusText := "⏳ Расшифровываю..."
	if queuePos > 1 {
		statusText = fmt.Sprintf("⏳ В очереди (позиция %d), подожди немного...", queuePos)
	}

	jobCtx, cancel := context.WithCancel(ctx)
	b.cancels.Store(jobID, cancel)
	keyboard := cancelKeyboard(jobID)
	b.edit(msg.Chat.ID, statusMsgID, statusText, &keyboard)

	go b.pollAndUpdate(jobCtx, cancel, msg, statusMsgID, jobID, preset)
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

func (b *Bot) edit(chatID int64, msgID int, text string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	textRunes := len([]rune(text))
	slog.Info("editing message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := tgbotapi.NewEditMessageText(chatID, msgID, text)
	cfg.ReplyMarkup = keyboard
	if _, err := b.api.Send(cfg); err != nil {
		if isMessageNotModifiedError(err) {
			return
		}
		slog.Error("edit message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
}

// editFinal edits the message text and removes the inline keyboard.
func (b *Bot) editFinal(chatID int64, msgID int, text string) {
	textRunes := len([]rune(text))
	slog.Info("editing final message", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes)
	cfg := tgbotapi.NewEditMessageText(chatID, msgID, text)
	if _, err := b.api.Send(cfg); err != nil {
		if isMessageNotModifiedError(err) {
			return
		}
		slog.Error("edit final message failed", "chat_id", chatID, "msg_id", msgID, "text_runes", textRunes, "error", err)
	}
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
		return fmt.Sprintf("⏳ Скачиваю %s... %.0f%%", noun, percent)
	}
	return fmt.Sprintf("⏳ Скачиваю %s... %s", noun, formatBytes(downloaded))
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
