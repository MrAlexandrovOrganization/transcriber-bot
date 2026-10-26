package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/whisper"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * pollInterval):
		t.Fatal("timed out waiting for queued work")
		var zero T
		return zero
	}
}

func TestChatQueuePreservesOrderWithoutBlockingOtherChats(t *testing.T) {
	var queue chatQueue
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	completed := make(chan int, 5)
	queue.enqueue(7, func() { close(firstStarted); <-release; completed <- 1 })
	receive(t, firstStarted)
	for i := 2; i <= 5; i++ {
		queue.enqueue(7, func() { completed <- i })
	}
	otherChat := make(chan struct{})
	queue.enqueue(8, func() { close(otherChat) })
	receive(t, otherChat)
	select {
	case got := <-completed:
		t.Fatalf("task %d overtook the blocked first task", got)
	default:
	}
	close(release)
	for want := 1; want <= 5; want++ {
		if got := receive(t, completed); got != want {
			t.Fatalf("completed task %d, want %d", got, want)
		}
	}
}

type batchBackend struct {
	pb.UnimplementedTranscriptionServiceServer
	submitted chan string
	firstDone <-chan struct{}
	first     *pb.StatusResponse
}

func (s *batchBackend) Submit(stream grpc.ClientStreamingServer[pb.TranscribeChunk, pb.SubmitResponse]) error {
	var audio strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		audio.Write(chunk.Data)
	}
	jobID := audio.String()
	s.submitted <- jobID
	if jobID == "submit-error" {
		return status.Error(codes.Internal, "synthetic submit failure")
	}
	return stream.SendAndClose(&pb.SubmitResponse{JobId: jobID, QueuePosition: 3})
}

func (s *batchBackend) GetStatus(_ context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	if req.JobId == "1" {
		select {
		case <-s.firstDone:
			return s.first, nil
		default:
			return &pb.StatusResponse{Status: pb.JobStatus_RUNNING}, nil
		}
	}
	return &pb.StatusResponse{Status: pb.JobStatus_DONE, Text: "text " + req.JobId}, nil
}

func startBatchBackend(t *testing.T, backend *batchBackend) *whisper.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterTranscriptionServiceServer(server, backend)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := whisper.NewClient(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestMediaBatchRepliesAsJobsFinish(t *testing.T) {
	for _, first := range []struct {
		name     string
		response *pb.StatusResponse
		text     string
	}{
		{"done", &pb.StatusResponse{Status: pb.JobStatus_DONE, Text: "text 1"}, "<blockquote>text 1</blockquote>"},
		{"failed", &pb.StatusResponse{Status: pb.JobStatus_FAILED, Error: "synthetic failure"}, "Произошла ошибка при расшифровке."},
		{"cancelled", &pb.StatusResponse{Status: pb.JobStatus_FAILED, Error: "cancelled"}, "❌ Отменено."},
	} {
		t.Run(first.name, func(t *testing.T) {
			t.Parallel()
			releaseFirst := make(chan struct{})
			backend := &batchBackend{submitted: make(chan string, 10), firstDone: releaseFirst, first: first.response}
			client := startBatchBackend(t, backend)
			files := make(map[string]string)
			for i := 1; i <= 5; i++ {
				id := strconv.Itoa(i)
				files[id] = filepath.Join(t.TempDir(), id+".ogg")
				if err := os.WriteFile(files[id], []byte(id), 0600); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan telegramRequest, 200)
			b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
				method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				requests <- telegramRequest{method: method, body: body}
				if method == "getFile" {
					id := body["file_id"].(string)
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": id, "file_path": files[id]}})
					return
				}
				msgID := 42
				if method == "sendMessage" {
					msgID = 100 + int(body["reply_parameters"].(map[string]any)["message_id"].(float64))
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":7,"type":"private"}}}`, msgID)
			})
			b.client = client
			t.Cleanup(func() {
				b.cancels.Range(func(_, cancel any) bool { cancel.(context.CancelFunc)(); return true })
			})
			for i := 1; i <= 5; i++ {
				b.dispatchUpdate(telego.Update{UpdateID: i, Message: &telego.Message{
					MessageID: i, Chat: telego.Chat{ID: 7, Type: "private"}, From: &telego.User{ID: 7}, Voice: &telego.Voice{FileID: strconv.Itoa(i)},
				}})
			}
			// Duplicate delivery must not add another status or backend job.
			b.dispatchUpdate(telego.Update{UpdateID: 6, Message: &telego.Message{
				MessageID: 1, Chat: telego.Chat{ID: 7, Type: "private"}, From: &telego.User{ID: 7}, Voice: &telego.Voice{FileID: "1"},
			}})
			for want := 1; want <= 5; want++ {
				if got := receive(t, backend.submitted); got != strconv.Itoa(want) {
					t.Fatalf("backend received %q, want %d", got, want)
				}
			}
			// All later results must reach Telegram while the first job is
			// still running, without a separate notification about waiting.
			initial := 0
			finished := make(map[int]bool)
			for len(finished) < 4 {
				r := receive(t, requests)
				text, _ := r.body["text"].(string)
				if strings.Contains(text, "ожидаю предыдущие сообщения") {
					t.Fatalf("unexpected waiting notification: %+v", r)
				}
				if r.method == "sendMessage" {
					initial++
					if got := int(r.body["reply_parameters"].(map[string]any)["message_id"].(float64)); got != initial {
						t.Fatalf("initial reply to %d, want %d", got, initial)
					}
				}
				if strings.HasPrefix(text, "<blockquote>") {
					id := int(r.body["message_id"].(float64)) - 100
					if id < 2 || id > 5 || finished[id] || text != fmt.Sprintf("<blockquote>text %d</blockquote>", id) {
						t.Fatalf("unexpected early result: %+v", r)
					}
					if r.body["reply_markup"] != nil || r.body["parse_mode"] != "HTML" {
						t.Fatalf("invalid final result formatting: %+v", r)
					}
					finished[id] = true
				}
			}
			if initial != 5 {
				t.Fatalf("initial replies = %d, want 5", initial)
			}
			close(releaseFirst)
			for {
				r := receive(t, requests)
				text, _ := r.body["text"].(string)
				if strings.Contains(text, "ожидаю предыдущие сообщения") {
					t.Fatalf("unexpected waiting notification: %+v", r)
				}
				if text != first.text && !strings.HasPrefix(text, "<blockquote>") {
					continue
				}
				if text != first.text || r.body["message_id"] != float64(101) || r.body["reply_markup"] != nil {
					t.Fatalf("first job's final reply = %+v, want %q on message 101", r, first.text)
				}
				break
			}
		})
	}
}

func TestUploadSlotsWaitInsteadOfRejectingFiles(t *testing.T) {
	started := make(chan struct{}, 3)
	finals := make(chan string, 3)
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			started <- struct{}{}
			<-release
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"synthetic download failure"}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if text, _ := body["text"].(string); strings.HasPrefix(text, "❌") {
			finals <- text
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"date":1,"chat":{"id":7,"type":"private"}}}`)
	})
	completed := make(chan struct{}, 3)
	for i := 1; i <= 3; i++ {
		go func() {
			b.processFile(context.Background(), &telego.Message{MessageID: i, Chat: telego.Chat{ID: int64(i)}}, 100+i, "audio", "ogg", "голосовое", "voice")
			completed <- struct{}{}
		}()
	}
	receive(t, started)
	receive(t, started)
	select {
	case <-completed:
		t.Fatal("file rejected while uploads were busy")
	default:
	}
	unblock.Do(func() { close(release) })
	receive(t, started)
	for range 3 {
		receive(t, completed)
		if text := receive(t, finals); !strings.Contains(text, "Не удалось скачать") {
			t.Fatalf("expected download attempt, got %q", text)
		}
	}
}

func TestSubmitFailureEditsStatus(t *testing.T) {
	backend := &batchBackend{submitted: make(chan string, 1)}
	client := startBatchBackend(t, backend)
	path := filepath.Join(t.TempDir(), "audio.ogg")
	if err := os.WriteFile(path, []byte("submit-error"), 0600); err != nil {
		t.Fatal(err)
	}
	edits := make(chan string, 10)
	b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "audio", "file_path": path}})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		edits <- body["text"].(string)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"date":1,"chat":{"id":7,"type":"private"}}}`)
	})
	b.client = client
	b.processFile(context.Background(), &telego.Message{Chat: telego.Chat{ID: 7}}, 42, "audio", "ogg", "голосовое", "voice")
	var final string
	for len(edits) > 0 {
		final = <-edits
	}
	if final != "Произошла ошибка при отправке на расшифровку." {
		t.Fatalf("submit failure status = %q", final)
	}
}

func TestLocalCancellationEditsStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, requests := fakeTelegram(t, nil)
	b.pollAndUpdate(ctx, cancel, &telego.Message{Chat: telego.Chat{ID: 7}}, 42, "cancelled-job", "voice")
	if r := receive(t, requests); r.method != "editMessageText" || r.body["text"] != "❌ Отменено." || r.body["reply_markup"] != nil {
		t.Fatalf("cancelled poll status = %+v", r)
	}
}
