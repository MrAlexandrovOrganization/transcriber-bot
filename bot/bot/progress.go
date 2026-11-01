package bot

import (
	"context"
	"time"

	"transcriber-bot/store"
	"transcriber-bot/tgfmt"
)

// Progress is disposable UI. A slow Telegram endpoint must not stall the audio
// stream or exhaust the backend upload deadline. Only the latest update matters.
func (b *Bot) uploadProgress(parent context.Context, job store.Job) (func(string), func()) {
	ctx, cancel := context.WithCancel(parent)
	updates := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		latest, last := "", ""
		for {
			select {
			case <-ctx.Done():
				return
			case latest = <-updates:
			case <-ticker.C:
				if latest == "" || latest == last {
					continue
				}
				editCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				_, err := b.api.EditMessageText(editCtx, editParams(job.ChatID, job.StatusMessageID, tgfmt.Escape(latest)))
				stop()
				if err == nil || isMessageNotModifiedError(err) {
					last = latest
				}
			}
		}
	}()
	report := func(text string) {
		select {
		case updates <- text:
			return
		default:
		}
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- text:
		default:
		}
	}
	// Join before releasing the job lock, so old progress cannot overwrite a result.
	return report, func() { cancel(); <-done }
}
