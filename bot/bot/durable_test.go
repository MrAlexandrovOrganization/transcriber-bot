package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
	"transcriber-bot/internal/testdb"
	"transcriber-bot/resources"
	"transcriber-bot/store"
	"transcriber-bot/whisper"
)

type durableBackend struct {
	pb.UnimplementedTranscriptionServiceServer
	mu                               sync.Mutex
	jobs                             map[string]*pb.StatusResponse
	requests                         map[string]string
	accepted, acks, cancels          int
	loseSubmit, loseAck, earlyReplay bool
	beforeAck                        func()
}

func (s *durableBackend) Submit(stream grpc.ClientStreamingServer[pb.TranscribeChunk, pb.SubmitResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.RequestId == "" || !first.RetainUntilAck {
		return status.Error(codes.InvalidArgument, "durable fields missing")
	}
	s.mu.Lock()
	existing := s.requests[first.RequestId]
	early := s.earlyReplay
	s.mu.Unlock()
	if existing != "" && early {
		return stream.SendAndClose(&pb.SubmitResponse{JobId: existing})
	}
	var audio strings.Builder
	audio.Write(first.Data)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.requests[first.RequestId]
	if id == "" {
		id = "job-" + audio.String()
		s.requests[first.RequestId] = id
		s.jobs[id] = &pb.StatusResponse{JobId: id, Status: pb.JobStatus_DONE, Text: "text " + audio.String()}
		s.accepted++
	}
	if s.loseSubmit {
		s.loseSubmit = false
		return status.Error(codes.Unavailable, "synthetic accepted response lost")
	}
	return stream.SendAndClose(&pb.SubmitResponse{JobId: id, QueuePosition: 1})
}
func (s *durableBackend) GetStatus(_ context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.jobs[r.JobId]
	if v == nil {
		return nil, status.Error(codes.NotFound, "synthetic missing job")
	}
	return v, nil
}
func (s *durableBackend) Cancel(_ context.Context, r *pb.CancelRequest) (*pb.CancelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	if v := s.jobs[r.JobId]; v != nil {
		v.Status = pb.JobStatus_FAILED
		v.Error = "cancelled"
	}
	return &pb.CancelResponse{Cancelled: true}, nil
}
func (s *durableBackend) Ack(_ context.Context, r *pb.AckRequest) (*pb.AckResponse, error) {
	if s.beforeAck != nil {
		s.beforeAck()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks++
	if s.loseAck {
		s.loseAck = false
		delete(s.jobs, r.JobId)
		return nil, status.Error(codes.Unavailable, "synthetic ack response lost")
	}
	if s.jobs[r.JobId] == nil {
		return nil, status.Error(codes.NotFound, "already acknowledged and cleaned")
	}
	return &pb.AckResponse{Acknowledged: true}, nil
}
func backendClient(t *testing.T, s *durableBackend) *whisper.Client {
	t.Helper()
	s.jobs = make(map[string]*pb.StatusResponse)
	s.requests = make(map[string]string)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterTranscriptionServiceServer(server, s)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	client, err := whisper.NewClient(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func openBotStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func sqlConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}
func media(id int) telego.Update {
	return telego.Update{UpdateID: id, Message: &telego.Message{MessageID: id, Chat: telego.Chat{ID: 7, Type: "private"}, From: &telego.User{ID: 7}, Voice: &telego.Voice{FileID: fmt.Sprint(id)}}}
}
func persist(t *testing.T, b *Bot, updates ...telego.Update) {
	t.Helper()
	p, err := b.chats.OpenPoller(context.Background(), b.botID)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	offset, err := p.Cursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = b.persistBatch(context.Background(), p, offset, updates); err != nil {
		t.Fatal(err)
	}
}
func admit(t *testing.T, b *Bot) {
	t.Helper()
	for {
		worked, err := b.chats.ProcessInbox(context.Background(), b.botID, func(_ int, data json.RawMessage) (*store.Job, error) {
			var update telego.Update
			if err := json.Unmarshal(data, &update); err != nil {
				return nil, err
			}
			return b.acceptUpdate(context.Background(), update)
		})
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
}
func step(t *testing.T, b *Bot, state string) error {
	t.Helper()
	worked, err := b.chats.WithWork(context.Background(), b.botID, state, func(w *store.Work) error { return b.step(context.Background(), w) })
	if !worked {
		t.Fatalf("no ready work for stage %s", state)
	}
	return err
}
func mustStep(t *testing.T, b *Bot, state string) {
	t.Helper()
	if err := step(t, b, state); err != nil {
		t.Fatal(state, err)
	}
}

type fakeDelivery struct {
	method, text string
	message      int
}
type telegramFixture struct {
	mu                                                 sync.Mutex
	files                                              map[string]string
	requests                                           []fakeDelivery
	nextID                                             int
	failSend, failDocument, notModified, deletedStatus bool
}

func (f *telegramFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
		var body map[string]any
		if method == "sendDocument" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("document")
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(file)
			file.Close()
			body = map[string]any{"text": string(data)}
		} else if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if method == "getFile" {
			id, _ := body["file_id"].(string)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": id, "file_path": f.files[id]}})
			return
		}
		text, _ := body["text"].(string)
		message, _ := body["message_id"].(float64)
		f.requests = append(f.requests, fakeDelivery{method, text, int(message)})
		if method == "editMessageText" && f.notModified {
			f.notModified = false
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
			return
		}
		if method == "editMessageText" && f.deletedStatus {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
			return
		}
		if method == "sendMessage" && f.failSend {
			f.failSend = false
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"synthetic rate limit","parameters":{"retry_after":1}}`)
			return
		}
		if method == "sendDocument" && f.failDocument {
			f.failDocument = false
			fmt.Fprint(w, `{"ok":false,"error_code":503,"description":"synthetic document failure"}`)
			return
		}
		f.nextID++
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":7,"type":"private"}}}`, 100+f.nextID)
	}
}
func fixture(t *testing.T, backend *durableBackend) (*Bot, string, *telegramFixture) {
	t.Helper()
	dsn := testdb.New(t)
	client := backendClient(t, backend)
	f := &telegramFixture{files: make(map[string]string)}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprint(i)
		path := filepath.Join(t.TempDir(), id+".ogg")
		if err := os.WriteFile(path, []byte(id), 0600); err != nil {
			t.Fatal(err)
		}
		f.files[id] = path
	}
	b, _ := fakeTelegram(t, f.handler(t))
	b.chats = openBotStore(t, dsn)
	b.client = client
	return b, dsn, f
}
func restart(t *testing.T, b *Bot, dsn string) *Bot {
	t.Helper()
	b.chats.Close()
	// Discard all per-process Bot fields. Telegram and backend servers remain external.
	next, err := New(b.cfg, b.client, openBotStore(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func jobState(t *testing.T, dsn string, id int) string {
	t.Helper()
	conn := sqlConn(t, dsn)
	var state string
	if err := conn.QueryRow(context.Background(), "SELECT state FROM transcription_jobs WHERE update_id=$1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDurableRestartAtEveryBoundaryAndLostSubmitResponse(t *testing.T) {
	backend := &durableBackend{loseSubmit: true, earlyReplay: true}
	b, dsn, fixtureAPI := fixture(t, backend)
	persist(t, b, media(1))
	b = restart(t, b, dsn) // intake is durable before any status/submit
	admit(t, b)
	b = restart(t, b, dsn)
	fixtureAPI.failSend = true
	if err := step(t, b, "status"); err == nil {
		t.Fatal("expected initial status failure")
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "status")
	b = restart(t, b, dsn)
	if err := step(t, b, "upload"); err == nil {
		t.Fatal("expected lost Submit response")
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "upload")
	b = restart(t, b, dsn)
	backend.beforeAck = func() {
		conn := sqlConn(t, dsn)
		var result bool
		var parts int
		if err := conn.QueryRow(context.Background(), "SELECT result IS NOT NULL FROM transcription_jobs WHERE update_id=1").Scan(&result); err != nil {
			t.Error(err)
		}
		conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM transcription_deliveries").Scan(&parts)
		if !result || parts != 1 {
			t.Error("Ack before result and outbox committed")
		}
	}
	mustStep(t, b, "poll")
	b = restart(t, b, dsn)
	mustStep(t, b, "deliver")
	if state := jobState(t, dsn, 1); state != "done" {
		t.Fatal(state)
	}
	if backend.accepted != 1 {
		t.Fatalf("accepted duplicate backend jobs: %d", backend.accepted)
	}
	duplicate := media(2)
	duplicate.Message.MessageID = 1
	persist(t, b, duplicate)
	admit(t, b)
	conn := sqlConn(t, dsn)
	var count int
	conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM transcription_jobs").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate job after restart")
	}
}
func TestDurablePartialDeliveryAndAckRecovery(t *testing.T) {
	backend := &durableBackend{loseAck: true}
	b, dsn, f := fixture(t, backend)
	persist(t, b, media(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	backend.mu.Lock()
	backend.jobs["job-1"].Text = strings.Repeat("<&>", maxMsgRunes)
	backend.mu.Unlock()
	mustStep(t, b, "poll")
	if err := step(t, b, "deliver"); err == nil {
		t.Fatal("expected ambiguous Ack")
	}
	// First part was delivered despite backend Ack outage. Only remaining parts retry.
	b = restart(t, b, dsn)
	f.failSend = true
	if err := step(t, b, "deliver"); err == nil {
		t.Fatal("expected Telegram retry")
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "deliver")
	mustStep(t, b, "deliver")
	if state := jobState(t, dsn, 1); state != "done" {
		t.Fatal(state)
	}
	edits, sends := 0, 0
	for _, r := range f.requests {
		if r.method == "editMessageText" && strings.HasPrefix(r.text, "<blockquote>") {
			edits++
			if strings.Contains(r.text, "<&>") {
				t.Fatal("unsafe HTML")
			}
		}
		if r.method == "sendMessage" {
			sends++
		}
	}
	if edits != 1 || sends != 4 {
		t.Fatalf("confirmed parts replayed / missing retries: edits %d sends %d", edits, sends)
	}
	if backend.acks != 2 {
		t.Fatalf("ambiguous Ack recovery: %d", backend.acks)
	}
}
func TestDurableDeletedMessageAndNotModified(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			b, dsn, f := fixture(t, &durableBackend{})
			persist(t, b, media(1))
			admit(t, b)
			mustStep(t, b, "status")
			mustStep(t, b, "upload")
			mustStep(t, b, "poll")
			f.deletedStatus = deleted
			f.notModified = !deleted
			mustStep(t, b, "deliver")
			if state := jobState(t, dsn, 1); state != "done" {
				t.Fatal(state)
			}
			count := 0
			for _, r := range f.requests {
				if r.method == "sendMessage" {
					count++
				}
			}
			want := 1
			if deleted {
				want = 2
			}
			if count != want {
				t.Fatal("missing fallback or duplicate for not-modified", count)
			}
		})
	}
}
func TestDurableLectureDocumentRetry(t *testing.T) {
	b, dsn, f := fixture(t, &durableBackend{})
	if err := b.chats.SetPreset(context.Background(), b.botID, 7, "lecture"); err != nil {
		t.Fatal(err)
	}
	persist(t, b, media(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	mustStep(t, b, "poll")
	mustStep(t, b, "deliver")
	f.failDocument = true
	if err := step(t, b, "deliver"); err == nil {
		t.Fatal("document error was ignored")
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "deliver")
	if state := jobState(t, dsn, 1); state != "done" {
		t.Fatal(state)
	}
	edits, docs := 0, 0
	for _, r := range f.requests {
		if r.method == "editMessageText" && r.text == resources.Ready {
			edits++
		}
		if r.method == "sendDocument" {
			docs++
		}
	}
	if edits != 1 || docs != 2 {
		t.Fatal("lecture progress not persisted", edits, docs)
	}
}
func TestLaterResultsAreDeliveredBeforeEarlierRunningJob(t *testing.T) {
	backend := &durableBackend{}
	b, _, f := fixture(t, backend)
	persist(t, b, media(1), media(2))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	mustStep(t, b, "upload")
	backend.mu.Lock()
	backend.jobs["job-1"].Status = pb.JobStatus_RUNNING
	backend.mu.Unlock()
	mustStep(t, b, "poll")
	mustStep(t, b, "poll")
	mustStep(t, b, "deliver")
	if r := f.requests[len(f.requests)-1]; r.text != "<blockquote>text 2</blockquote>" {
		t.Fatal("later result blocked", r)
	}
}
func TestPersistentCancellationAfterRestartIsChatScoped(t *testing.T) {
	backend := &durableBackend{}
	b, dsn, f := fixture(t, backend)
	persist(t, b, media(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	if err := b.chats.RequestCancel(context.Background(), b.botID, "job-1", 999); err != nil {
		t.Fatal(err)
	}
	conn := sqlConn(t, dsn)
	var canceled bool
	conn.QueryRow(context.Background(), "SELECT cancel_requested FROM transcription_jobs").Scan(&canceled)
	if canceled {
		t.Fatal("another chat canceled job")
	}
	if err := b.chats.RequestCancel(context.Background(), b.botID, "job-1", 7); err != nil {
		t.Fatal(err)
	}
	b = restart(t, b, dsn)
	mustStep(t, b, "poll")
	mustStep(t, b, "deliver")
	if backend.cancels != 1 || f.requests[len(f.requests)-1].text != resources.Cancelled {
		t.Fatal("cancel lost after restart")
	}
}
func TestResultFormattingAndRetryBackoff(t *testing.T) {
	parts := resultParts(store.Job{Preset: "voice"}, &whisper.JobResult{Status: pb.JobStatus_DONE, Text: "<unsafe>&"})
	if len(parts) != 1 || !parts[0].Quoted || parts[0].Text != "<unsafe>&" {
		t.Fatal(parts)
	}
	if retryDelay(0) != 2*time.Second || retryDelay(100) != 256*time.Second {
		t.Fatal("retry backoff not bounded")
	}
	if retryAfter(&telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 3600}}, 0) != time.Hour {
		t.Fatal("Telegram retry_after ignored")
	}
	if !isMissingMessage(errors.New("Bad Request: message to edit not found")) {
		t.Fatal("missing edit not recognized")
	}
}

// Exercise the actual intake and all worker loops. A restart during blocked
// downloads keeps the cursor, initial messages and pending media in PostgreSQL.
func TestRunContextRestartKeepsBatchAndBoundsUploads(t *testing.T) {
	dsn := testdb.New(t)
	backend := &durableBackend{}
	client := backendClient(t, backend)
	f := &telegramFixture{files: map[string]string{}}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint(i)
		path := filepath.Join(t.TempDir(), id+".ogg")
		os.WriteFile(path, []byte(id), 0600)
		f.files[id] = path
	}
	started := make(chan struct{}, 10)
	offsets := make(chan int, 10)
	release := make(chan struct{})
	var active, maxActive int
	var mu sync.Mutex
	base := f.handler(t)
	b, _ := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var body struct {
				Offset int `json:"offset"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			offsets <- body.Offset
			if body.Offset == 0 {
				batch := []telego.Update{media(1), media(2), media(3)}
				for i := range batch {
					batch[i].Message.Chat.ID = -int64(i + 7)
					batch[i].Message.Chat.Type = "group"
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": batch})
				return
			}
			// An acknowledging offset is legal only after the entire batch committed.
			conn, err := pgx.Connect(r.Context(), dsn)
			if err != nil {
				t.Error(err)
				return
			}
			var count int
			err = conn.QueryRow(r.Context(), "SELECT COUNT(*) FROM bot_inbox").Scan(&count)
			conn.Close(context.Background())
			if err != nil || count != 3 {
				t.Error("getUpdates acknowledged uncommitted intake", count, err)
			}
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			mu.Lock()
			active++
			maxActive = max(maxActive, active)
			mu.Unlock()
			defer func() { mu.Lock(); active--; mu.Unlock() }()
			started <- struct{}{}
			select {
			case <-release:
				base(w, r)
			case <-r.Context().Done():
			}
		default:
			base(w, r)
		}
	})
	b.client = client
	b.chats = openBotStore(t, dsn)
	run := func(b *Bot) (context.CancelFunc, <-chan struct{}, *error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var runErr error
		go func() { runErr = b.RunContext(ctx); close(done) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("worker shutdown exceeded test budget")
			}
		})
		return cancel, done, &runErr
	}
	receive := func(ch <-chan int) int {
		select {
		case v := <-ch:
			return v
		case <-time.After(5 * time.Second):
			t.Fatal("intake stalled")
			return 0
		}
	}
	cancel, done, runErr := run(b)
	if offset := receive(offsets); offset != 0 {
		t.Fatal("initial cursor", offset)
	}
	if offset := receive(offsets); offset != 4 {
		t.Fatal("persisted offset", offset)
	}
	// Two uploads proceed, while the third remains durable and waiting.
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("two upload slots did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("third upload bypassed concurrency limit")
	default:
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		count := 0
		for _, r := range f.requests {
			if r.method == "sendMessage" {
				count++
			}
		}
		f.mu.Unlock()
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status admission was blocked by upload")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
	if *runErr != nil {
		t.Fatal(*runErr)
	}
	b = restart(t, b, dsn)
	close(release)
	cancel2, done2, runErr2 := run(b)
	if offset := receive(offsets); offset != 4 {
		t.Fatal("cursor reset after restart", offset)
	}
	conn := sqlConn(t, dsn)
	deadline = time.Now().Add(8 * time.Second)
	for {
		var count int
		if err := conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM transcription_jobs WHERE state='done'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart did not finish all pending jobs", count)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-done2
	if *runErr2 != nil {
		t.Fatal(*runErr2)
	}
	mu.Lock()
	peak := maxActive
	mu.Unlock()
	if peak != 2 {
		t.Fatalf("upload concurrency exceeded: %d", peak)
	}
	backend.mu.Lock()
	accepted := backend.accepted
	backend.mu.Unlock()
	if accepted != 3 {
		t.Fatal("lost or duplicated submissions", accepted)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	statuses := 0
	for _, r := range f.requests {
		if r.method == "sendMessage" {
			statuses++
		}
	}
	if statuses != 3 {
		t.Fatal("status messages replayed after restart", statuses)
	}
}

func TestMissingRetainedJobBecomesDurableErrorInsteadOfHanging(t *testing.T) {
	backend := &durableBackend{}
	b, dsn, f := fixture(t, backend)
	persist(t, b, media(1))
	admit(t, b)
	mustStep(t, b, "status")
	mustStep(t, b, "upload")
	backend.mu.Lock()
	delete(backend.jobs, "job-1")
	backend.mu.Unlock()
	mustStep(t, b, "poll")
	b = restart(t, b, dsn)
	mustStep(t, b, "deliver")
	if jobState(t, dsn, 1) != "done" || f.requests[len(f.requests)-1].text != resources.MissingJob {
		t.Fatal("missing result was not reported durably")
	}
}
