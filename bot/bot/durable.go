package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/resources"
	"transcriber-bot/store"
	"transcriber-bot/tgfmt"
	"transcriber-bot/whisper"
)

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RunContext has one durable intake and bounded stage workers. Losing the poller
// lock connection stops this instance; per-job connection locks fence workers.
func (b *Bot) RunContext(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	poller, err := b.chats.OpenPoller(ctx, b.botID)
	if err != nil {
		return err
	}
	defer poller.Close()
	var workers sync.WaitGroup
	start := func(run func()) { workers.Add(1); go func() { defer workers.Done(); run() }() }
	start(func() { b.inboxLoop(ctx) })
	for state, count := range map[string]int{"status": 2, "upload": 2, "poll": 2, "deliver": 2} {
		for range count {
			start(func() { b.workLoop(ctx, state) })
		}
	}
	err = b.intake(ctx, poller)
	cancel()
	workers.Wait()
	if parent.Err() != nil {
		return nil
	}
	return err
}

func (b *Bot) intake(ctx context.Context, poller *store.Poller) error {
	for ctx.Err() == nil {
		offset, err := poller.Cursor(ctx)
		if err != nil {
			return fmt.Errorf("poller database session lost")
		}
		updates, err := b.api.GetUpdates(ctx, &telego.GetUpdatesParams{Offset: offset, Timeout: 60, AllowedUpdates: []string{"message", "callback_query", "my_chat_member"}})
		if err != nil {
			slog.Warn("get updates failed", "error", maskBotToken(err, b.cfg.BotToken))
			if !wait(ctx, 3*time.Second) {
				break
			}
			continue
		}
		if err = b.persistBatch(ctx, poller, offset, updates); err != nil {
			return fmt.Errorf("persist updates failed")
		}
	}
	return ctx.Err()
}
func (b *Bot) persistBatch(ctx context.Context, poller *store.Poller, offset int, updates []telego.Update) error {
	records := make([]store.Update, 0, len(updates))
	for _, update := range updates {
		if update.UpdateID < offset {
			continue
		}
		payload, err := json.Marshal(update)
		if err != nil {
			return err
		}
		records = append(records, store.Update{ID: update.UpdateID, Payload: payload})
	}
	next := offset
	for _, record := range records {
		next = max(next, record.ID+1)
	}
	return poller.Accept(ctx, records, next)
}
func (b *Bot) inboxLoop(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := b.chats.ProcessInbox(ctx, b.botID, func(id int, payload json.RawMessage) (*store.Job, error) {
			var update telego.Update
			if err := json.Unmarshal(payload, &update); err != nil {
				return nil, err
			}
			return b.acceptUpdate(ctx, update)
		})
		if err != nil {
			slog.Warn("durable admission failed")
		}
		if !worked || err != nil {
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}
func (b *Bot) acceptUpdate(ctx context.Context, u telego.Update) (*store.Job, error) {
	if cb := u.CallbackQuery; cb != nil {
		if strings.HasPrefix(cb.Data, "cancel:") {
			if cb.From.ID != b.cfg.RootID {
				b.answerCallback(ctx, cb.ID, resources.CancelDenied)
				return nil, nil
			}
			if cb.Message != nil {
				if err := b.chats.RequestCancel(ctx, b.botID, strings.TrimPrefix(cb.Data, "cancel:"), cb.Message.GetChat().ID); err != nil {
					return nil, err
				}
			}
			b.answerCallback(ctx, cb.ID, "")
			return nil, nil
		}
		if strings.HasPrefix(cb.Data, "preset:") {
			preset := strings.TrimPrefix(cb.Data, "preset:")
			if _, valid := presetLabels[preset]; valid {
				if err := b.chats.SetPreset(ctx, b.botID, cb.From.ID, preset); err != nil {
					return nil, err
				}
				b.handlePresetCallback(ctx, cb)
			}
		}
		return nil, nil
	}
	if member := u.MyChatMember; member != nil {
		if !isGroupChat(member.Chat) {
			return nil, nil
		}
		switch member.NewChatMember.MemberStatus() {
		case "member", "administrator", "restricted":
			_, err := b.chats.Add(ctx, member.Chat.ID, member.Chat.Title, member.From.ID)
			return nil, err
		default:
			return nil, b.chats.Remove(ctx, member.Chat.ID)
		}
	}
	msg := u.Message
	if msg == nil || msg.From == nil {
		return nil, nil
	}
	if isGroupChat(msg.Chat) {
		allowed, err := b.chats.Exists(ctx, msg.Chat.ID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			if msg.From.ID != b.cfg.RootID {
				return nil, nil
			}
			if _, err = b.chats.Add(ctx, msg.Chat.ID, msg.Chat.Title, msg.From.ID); err != nil {
				return nil, err
			}
		}
	} else if msg.From.ID != b.cfg.RootID {
		return nil, nil
	}
	preset, err := b.chats.Preset(ctx, b.botID, msg.From.ID)
	if err != nil {
		return nil, err
	}
	if messageCommand(msg) != "" {
		if !isGroupChat(msg.Chat) || msg.From.ID == b.cfg.RootID {
			b.userPreset.Store(msg.From.ID, preset)
			b.handleCommand(ctx, msg)
		}
		return nil, nil
	}
	if isGroupChat(msg.Chat) && msg.Voice == nil && msg.VideoNote == nil {
		return nil, nil
	}
	fileID, format := extractFile(msg)
	if fileID == "" {
		return nil, nil
	}
	return &store.Job{ChatID: msg.Chat.ID, UserID: msg.From.ID, MessageID: msg.MessageID, FileID: fileID, Format: format, Preset: resolvePreset(preset, msg), Noun: mediaNoun(msg)}, nil
}

func (b *Bot) workLoop(ctx context.Context, state string) {
	for ctx.Err() == nil {
		worked, err := b.chats.WithWork(ctx, b.botID, state, func(w *store.Work) error {
			ctx, span := otel.Tracer("transcriber-bot").Start(ctx, "transcription."+state)
			defer span.End()
			if err := b.step(ctx, w); err != nil {
				if ctx.Err() != nil {
					return err
				}
				slog.Warn("durable job stage will retry", "stage", state, "update_id", w.Job.UpdateID)
				return w.Schedule(ctx, retryAfter(err, w.Job.Attempts), "stage_failed")
			}
			return nil
		})
		if err != nil {
			slog.Warn("durable job store unavailable", "stage", state)
		}
		if !worked || err != nil {
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}
func retryDelay(attempt int) time.Duration {
	return time.Second * time.Duration(1<<min(max(attempt, 0)+1, 8))
}
func retryAfter(err error, attempt int) time.Duration {
	delay := retryDelay(attempt)
	var apiError *telegoapi.Error
	if errors.As(err, &apiError) && apiError.Parameters != nil {
		delay = max(delay, time.Duration(min(max(apiError.Parameters.RetryAfter, 0), 86400))*time.Second)
	}
	return delay
}
func sourceMessage(job store.Job) *telego.Message {
	return &telego.Message{MessageID: job.MessageID, Chat: telego.Chat{ID: job.ChatID}, From: &telego.User{ID: job.UserID}}
}
func (b *Bot) step(ctx context.Context, w *store.Work) error {
	j := w.Job
	switch j.State {
	case "status":
		message, err := b.sendInitialStatus(ctx, sourceMessage(j), j.Noun)
		if err != nil {
			return err
		}
		return w.Status(ctx, message.MessageID)
	case "upload":
		report, stop := b.uploadProgress(ctx, j)
		defer stop()
		report(fmt.Sprintf(resources.DownloadingMedia, j.Noun))
		reader, err := b.downloadFile(ctx, j.FileID, func(done, total int64) { report(formatDownloadStatus(done, total, j.Noun)) })
		if err != nil {
			return err
		}
		defer reader.Close()
		report(resources.UploadingMedia)
		id, _, err := b.client.SubmitDurable(ctx, reader, j.Format, buildOptions(j.Preset), j.RequestID)
		if err != nil {
			if status.Code(err) == codes.InvalidArgument {
				return b.saveResult(ctx, w, &whisper.JobResult{Status: pb.JobStatus_FAILED, Error: "upload_invalid"})
			}
			return err
		}
		return w.Submitted(ctx, id)
	case "poll":
		if j.CancelRequested {
			if _, err := b.client.CancelContext(ctx, j.JobID); err != nil {
				return err
			}
		}
		result, err := b.client.GetStatusContext(ctx, j.JobID)
		if status.Code(err) == codes.NotFound {
			return b.saveResult(ctx, w, &whisper.JobResult{Status: pb.JobStatus_FAILED, Error: "job_missing"})
		}
		if err != nil {
			return err
		}
		if result.Status == pb.JobStatus_DONE || result.Status == pb.JobStatus_FAILED {
			return b.saveResult(ctx, w, result)
		}
		text := resources.Queued
		if result.Status == pb.JobStatus_RUNNING {
			text = resources.Transcribing
			if result.ProgressPercent > 0 {
				text = fmt.Sprintf(resources.Progress, result.ProgressPercent)
			}
		}
		if text != j.ProgressText {
			params := editParams(j.ChatID, j.StatusMessageID, tgfmt.Escape(text))
			params.ReplyMarkup = cancelKeyboard(j.JobID)
			if _, err = b.api.EditMessageText(ctx, params); err == nil || isMessageNotModifiedError(err) {
				if err = w.Progress(ctx, text); err != nil {
					return err
				}
			}
		}
		return w.Schedule(ctx, pollInterval, "")
	case "deliver":
		return b.deliver(ctx, w)
	default:
		return errors.New("unknown durable job state")
	}
}
func resultParts(j store.Job, result *whisper.JobResult) []store.Delivery {
	if result.Status == pb.JobStatus_FAILED {
		text := resources.TranscriptionFailed
		switch result.Error {
		case "cancelled":
			text = resources.Cancelled
		case "job_missing":
			text = resources.MissingJob
		case "upload_invalid":
			text = resources.UploadFailed
		}
		return []store.Delivery{{Kind: "edit", Text: text}}
	}
	text := formatResult(result, j.Preset)
	if text == "" {
		text = resources.Silence
	}
	if j.Preset == "lecture" {
		return []store.Delivery{{Kind: "edit", Text: resources.Ready}, {Kind: "document", Text: text}}
	}
	var out []store.Delivery
	for i, part := range splitText(text) {
		kind := "send"
		if i == 0 {
			kind = "edit"
		}
		out = append(out, store.Delivery{Kind: kind, Text: part, Quoted: true})
	}
	return out
}
func (b *Bot) saveResult(ctx context.Context, w *store.Work, result *whisper.JobResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return w.Result(ctx, payload, resultParts(w.Job, result))
}
func (b *Bot) deliver(ctx context.Context, w *store.Work) error {
	// Delivery remains independent of backend availability once the result is local.
	var ackErr error
	if !w.Job.Acknowledged {
		ackErr = b.client.Ack(ctx, w.Job.JobID)
		if ackErr == nil {
			if err := w.Acknowledged(ctx); err != nil {
				return err
			}
		}
	}
	part, err := w.NextDelivery(ctx)
	if err != nil {
		return err
	}
	if part != nil {
		id, err := b.sendDelivery(ctx, w.Job, *part)
		if err != nil {
			return err
		}
		if err = w.Delivered(ctx, part.Part, id); err != nil {
			return err
		}
	}
	if ackErr != nil {
		return ackErr
	}
	return w.Complete(ctx)
}
func (b *Bot) sendDelivery(ctx context.Context, j store.Job, d store.Delivery) (int, error) {
	html := tgfmt.Escape(d.Text)
	if d.Quoted {
		html = quoteText(d.Text)
	}
	reply := &telego.ReplyParameters{MessageID: j.MessageID, ChatID: tu.ID(j.ChatID), AllowSendingWithoutReply: true}
	switch d.Kind {
	case "edit":
		_, err := b.api.EditMessageText(ctx, editParams(j.ChatID, j.StatusMessageID, html))
		if err == nil || isMessageNotModifiedError(err) {
			return j.StatusMessageID, nil
		}
		if !isMissingMessage(err) {
			return 0, err
		}
		// A deleted status message is replaced; the original reply may be deleted too.
		fallthrough
	case "send":
		params := messageParams(j.ChatID, html)
		params.ReplyParameters = reply
		msg, err := b.api.SendMessage(ctx, params)
		if err != nil {
			return 0, err
		}
		return msg.MessageID, nil
	case "document":
		msg, err := b.api.SendDocument(ctx, &telego.SendDocumentParams{ChatID: tu.ID(j.ChatID), Document: tu.FileFromBytes([]byte(d.Text), fmt.Sprintf(resources.LectureFileName, j.MessageID)), ReplyParameters: reply})
		if err != nil {
			return 0, err
		}
		return msg.MessageID, nil
	default:
		return 0, errors.New("unknown delivery kind")
	}
}
func isMissingMessage(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "message to edit not found") || strings.Contains(text, "message_id_invalid") || strings.Contains(text, "message can't be edited") || strings.Contains(text, "message can not be edited")
}
