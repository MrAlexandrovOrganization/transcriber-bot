package bot

import (
	"context"
	"testing"

	"github.com/mymmrac/telego"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/store"
	"transcriber-bot/whisper"
)

func groupMedia(id int) telego.Update {
	u := media(id)
	u.Message.Chat = telego.Chat{ID: -7, Type: "supergroup"}
	return u
}

func TestUnavailableGroupIsSkippedBeforeTelegramAndNeverResumes(t *testing.T) {
	for _, rpcFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_workers", true: "backend_down"}[rpcFailure], func(t *testing.T) {
			backend := &durableBackend{unavailable: true}
			if rpcFailure {
				backend.availabilityErr = status.Error(codes.Unavailable, "synthetic outage")
			}
			b, dsn, f := fixture(t, backend)
			persist(t, b, groupMedia(1))
			admit(t, b)
			mustStep(t, b, "status")
			b = restart(t, b, dsn)
			mustStep(t, b, "deliver")
			backend.unavailable, backend.availabilityErr = false, nil
			b = restart(t, b, dsn)
			if worked, err := b.chats.WithWork(context.Background(), b.botID, "status", func(*store.Work) error { t.Fatal("skipped job resumed"); return nil }); worked || err != nil {
				t.Fatal(worked, err)
			}
			if len(f.requests) != 0 || backend.accepted != 0 || jobState(t, dsn, 1) != "done" {
				t.Fatal("unavailable group produced output or uploaded media", f.requests)
			}
		})
	}
}

func TestGroupCancellationDeletesStatusDurablyWithoutConfirmation(t *testing.T) {
	backend := &durableBackend{}
	b, dsn, f := fixture(t, backend)
	persist(t, b, groupMedia(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	if err := b.chats.RequestCancel(context.Background(), b.botID, "job-1", -7); err != nil {
		t.Fatal(err)
	}
	f.failDelete = true
	if err := step(t, b, "poll"); err == nil {
		t.Fatal("expected transient deletion error")
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "poll")
	b = restart(t, b, dsn)
	f.missingDelete = true // Lost delete receipt / already deleted is successful.
	mustStep(t, b, "deliver")
	if jobState(t, dsn, 1) != "done" || backend.acks != 1 {
		t.Fatal("cancellation or acknowledgement was lost")
	}
	for i, r := range f.requests {
		if i == 0 && r.method == "sendMessage" {
			continue
		}
		if r.method != "deleteMessage" || r.text != "" {
			t.Fatal("cancellation left a message", r)
		}
	}
}

func TestGroupOutageAfterSubmissionPersistsCancellationAndHidesStatus(t *testing.T) {
	for _, scenario := range []string{"queued", "running", "backend_down"} {
		t.Run(scenario, func(t *testing.T) {
			backend := &durableBackend{}
			b, dsn, f := fixture(t, backend)
			persist(t, b, groupMedia(1))
			admit(t, b)
			mustStep(t, b, "status")
			mustStep(t, b, "upload")
			backend.jobs["job-1"].Status = pb.JobStatus_QUEUED
			if scenario == "running" {
				backend.jobs["job-1"].Status = pb.JobStatus_RUNNING
			}
			backend.unavailable = true
			if scenario == "backend_down" {
				backend.statusErr = status.Error(codes.Unavailable, "synthetic outage")
			}
			mustStep(t, b, "poll")
			conn := sqlConn(t, dsn)
			var canceled bool
			if err := conn.QueryRow(context.Background(), "SELECT cancel_requested FROM transcription_jobs").Scan(&canceled); err != nil || !canceled {
				t.Fatal("outage cancellation was not persisted", err)
			}
			if r := f.requests[len(f.requests)-1]; r.method != "deleteMessage" {
				t.Fatal("status was not hidden", r)
			}
			backend.unavailable, backend.statusErr = false, nil
			if _, err := conn.Exec(context.Background(), "UPDATE transcription_jobs SET next_attempt_at=now()"); err != nil {
				t.Fatal(err)
			}
			b = restart(t, b, dsn)
			mustStep(t, b, "poll")
			mustStep(t, b, "deliver")
			if backend.cancels != 1 || backend.acks != 1 || jobState(t, dsn, 1) != "done" {
				t.Fatal("backend work was abandoned after outage")
			}
		})
	}
}

func TestPrivateChatStillQueuesWithoutAvailableWorkers(t *testing.T) {
	backend := &durableBackend{unavailable: true}
	b, dsn, f := fixture(t, backend)
	persist(t, b, media(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	if backend.accepted != 1 || jobState(t, dsn, 1) != "poll" || len(f.requests) == 0 || f.requests[0].method != "sendMessage" {
		t.Fatal("private durable queue was suppressed")
	}
}

func TestGroupResultsAreDeliveredAndFailuresAreSilent(t *testing.T) {
	job := store.Job{GroupChat: true, Preset: "voice"}
	for _, code := range []string{"cancelled", "job_missing", "upload_invalid", "inference_failed"} {
		parts := resultParts(job, &whisper.JobResult{Status: pb.JobStatus_FAILED, Error: code})
		if len(parts) != 1 || parts[0].Kind != "delete" || parts[0].Text != "" {
			t.Fatal(code, parts)
		}
	}
	parts := resultParts(job, &whisper.JobResult{Status: pb.JobStatus_DONE, Text: "transcript"})
	if len(parts) != 1 || parts[0].Kind != "edit" || parts[0].Text != "transcript" {
		t.Fatal("successful group result suppressed", parts)
	}
	job.CancelRequested = true
	if parts := resultParts(job, &whisper.JobResult{Status: pb.JobStatus_DONE, Text: "transcript"}); parts[0].Kind != "delete" {
		t.Fatal("cancellation race restored group output", parts)
	}
}
