package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"transcriber-bot/internal/testdb"
)

const testBotID = int64(987654321)

func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func acceptJobs(t *testing.T, s *Store, jobs []Job) {
	t.Helper()
	ctx := context.Background()
	p, err := s.OpenPoller(ctx, testBotID)
	if err != nil {
		t.Fatal(err)
	}
	var batch []Update
	for i := range jobs {
		batch = append(batch, Update{ID: i + 1, Payload: json.RawMessage(`{"synthetic":true}`)})
	}
	if err = p.Accept(ctx, batch, len(batch)+1); err != nil {
		t.Fatal(err)
	}
	p.Close()
	for i := range jobs {
		worked, err := s.ProcessInbox(ctx, testBotID, func(int, json.RawMessage) (*Job, error) { return &jobs[i], nil })
		if err != nil || !worked {
			t.Fatalf("admit %d: %v %v", i, worked, err)
		}
	}
}
func testJob(chat int64, message int) Job {
	return Job{ChatID: chat, UserID: 7, MessageID: message, FileID: "synthetic-audio", Format: "ogg", Preset: "voice", Noun: "голосовое"}
}
func stage(t *testing.T, s *Store, state string, run func(*Work) error) bool {
	t.Helper()
	worked, err := s.WithWork(context.Background(), testBotID, state, run)
	if err != nil {
		t.Fatal(err)
	}
	return worked
}

func TestCursorAtomicityAndPollerOwnership(t *testing.T) {
	s := openStore(t, testdb.New(t))
	ctx := context.Background()
	p, err := s.OpenPoller(ctx, testBotID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenPoller(ctx, testBotID); err == nil {
		t.Fatal("second poller acquired singleton lock")
	}
	if err = p.Accept(ctx, []Update{{1, json.RawMessage(`{}`)}, {2, json.RawMessage(`not-json`)}}, 3); err == nil {
		t.Fatal("expected transaction failure")
	}
	offset, _ := p.Cursor(ctx)
	if offset != 0 {
		t.Fatalf("offset advanced before inbox commit: %d", offset)
	}
	var count int
	s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM bot_inbox").Scan(&count)
	if count != 0 {
		t.Fatal("partially committed inbox")
	}
	if err = p.Accept(ctx, []Update{{1, json.RawMessage(`{}`)}}, 2); err != nil {
		t.Fatal(err)
	}
	p.Close()
	restarted, err := s.OpenPoller(ctx, testBotID)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	offset, err = restarted.Cursor(ctx)
	if err != nil || offset != 2 {
		t.Fatalf("restart cursor: %d %v", offset, err)
	}
	if err = restarted.Accept(ctx, []Update{{1, json.RawMessage(`{}`)}}, 2); err != nil {
		t.Fatal(err)
	}
	s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM bot_inbox").Scan(&count)
	if count != 1 {
		t.Fatal("duplicate update persisted twice")
	}
}

func TestAbandonedPollerSessionExpiresWithoutLeakingTimeoutToWorkers(t *testing.T) {
	s := openStore(t, testdb.New(t))
	ctx := context.Background()
	p, err := s.OpenPoller(ctx, testBotID)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var timeout string
	if err = p.conn.QueryRow(ctx, "SHOW idle_session_timeout").Scan(&timeout); err != nil || timeout != "2min" {
		t.Fatalf("poller idle limit: %q %v", timeout, err)
	}
	if _, err = s.OpenPoller(ctx, testBotID); !errors.Is(err, ErrPollerOwned) {
		t.Fatalf("live poller ownership was bypassed: %v", err)
	}
	// Simulate a frozen/disconnected process with a shorter server-side limit.
	if _, err = p.conn.Exec(ctx, "SET idle_session_timeout='1s'"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		replacement, err := s.OpenPoller(ctx, testBotID)
		if err == nil {
			replacement.Close()
			break
		}
		if !errors.Is(err, ErrPollerOwned) || time.Now().After(deadline) {
			t.Fatalf("abandoned poller did not release ownership: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = p.Accept(ctx, []Update{{1, json.RawMessage(`{}`)}}, 2); err == nil {
		t.Fatal("expired poller could still commit updates")
	}
	if err = s.pool.QueryRow(ctx, "SHOW idle_session_timeout").Scan(&timeout); err != nil || timeout != "0" {
		t.Fatalf("poller timeout leaked into worker pool: %q %v", timeout, err)
	}
}
func TestAdmissionFailureCannotConsumeInbox(t *testing.T) {
	s := openStore(t, testdb.New(t))
	ctx := context.Background()
	p, _ := s.OpenPoller(ctx, testBotID)
	if err := p.Accept(ctx, []Update{{1, json.RawMessage(`{"file":"synthetic"}`)}}, 2); err != nil {
		t.Fatal(err)
	}
	p.Close()
	_, err := s.ProcessInbox(ctx, testBotID, func(int, json.RawMessage) (*Job, error) { return nil, errors.New("synthetic database interruption") })
	if err == nil {
		t.Fatal("expected admission failure")
	}
	worked, err := s.ProcessInbox(ctx, testBotID, func(_ int, payload json.RawMessage) (*Job, error) {
		if string(payload) != `{"file": "synthetic"}` {
			t.Errorf("payload lost: %s", payload)
		}
		j := testJob(7, 1)
		return &j, nil
	})
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	var payload string
	s.pool.QueryRow(ctx, "SELECT payload::text FROM bot_inbox").Scan(&payload)
	if payload != "{}" {
		t.Fatal("handled update retained unnecessary payload")
	}
}
func TestFIFOAndIndependentChatsSurviveRestart(t *testing.T) {
	dsn := testdb.New(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	acceptJobs(t, s, []Job{testJob(7, 1), testJob(7, 2), testJob(8, 3)})
	for stage(t, s, "status", func(w *Work) error { return w.Status(ctx, 100+int(w.Job.UpdateID)) }) {
	}
	held := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := s.WithWork(ctx, testBotID, "upload", func(w *Work) error {
			if w.Job.UpdateID != 1 {
				return errors.New("first upload out of order")
			}
			close(held)
			<-release
			return w.Submitted(ctx, "job-1")
		})
		finished <- err
	}()
	<-held
	if !stage(t, s, "upload", func(w *Work) error {
		if w.Job.UpdateID != 3 {
			t.Errorf("another chat blocked / FIFO bypassed: %d", w.Job.UpdateID)
		}
		return w.Submitted(ctx, "job-3")
	}) {
		t.Fatal("other chat was blocked")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	s.Close()
	restarted := openStore(t, dsn)
	if !stage(t, restarted, "upload", func(w *Work) error {
		if w.Job.UpdateID != 2 {
			t.Errorf("lost pending upload: %d", w.Job.UpdateID)
		}
		return w.Submitted(ctx, "job-2")
	}) {
		t.Fatal("pending work disappeared")
	}
	if !stage(t, restarted, "poll", func(w *Work) error { return w.Schedule(ctx, time.Hour, "") }) {
		t.Fatal("submitted work disappeared")
	}
}
func TestAtomicResultAndConnectionFencing(t *testing.T) {
	dsn := testdb.New(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	acceptJobs(t, s, []Job{testJob(7, 1)})
	stage(t, s, "status", func(w *Work) error { return w.Status(ctx, 42) })
	stage(t, s, "upload", func(w *Work) error { return w.Submitted(ctx, "backend-job") })
	stage(t, s, "poll", func(w *Work) error {
		if err := w.Result(ctx, json.RawMessage(`{"Text":"synthetic"}`), []Delivery{{Kind: "invalid", Text: "bad"}}); err == nil {
			t.Fatal("expected rollback")
		}
		j, err := readJob(ctx, w.conn, testBotID, 1)
		if err != nil || j.State != "poll" || j.Result != nil {
			t.Fatalf("result partially committed: %+v %v", j, err)
		}
		pid := w.conn.Conn().PgConn().PID()
		if _, err = s.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
			t.Fatal(err)
		}
		if err = w.Result(ctx, json.RawMessage(`{}`), []Delivery{{Kind: "edit", Text: "synthetic"}}); err == nil {
			t.Fatal("lost lock connection could commit stale result")
		}
		return nil
	})
	if !stage(t, s, "poll", func(w *Work) error {
		return w.Result(ctx, json.RawMessage(`{"Text":"synthetic"}`), []Delivery{{Kind: "edit", Text: "synthetic"}, {Kind: "send", Text: "part two"}})
	}) {
		t.Fatal("crashed attempt could not be recovered")
	}
	s.Close()
	s = openStore(t, dsn)
	stage(t, s, "deliver", func(w *Work) error {
		if w.Job.Acknowledged {
			t.Fatal("Ack advanced before caller persisted it")
		}
		d, err := w.NextDelivery(ctx)
		if err != nil || d.Part != 0 {
			t.Fatal(d, err)
		}
		if err = w.Delivered(ctx, 0, 42); err != nil {
			return err
		}
		return w.Acknowledged(ctx)
	})
	s.Close()
	s = openStore(t, dsn)
	stage(t, s, "deliver", func(w *Work) error {
		d, err := w.NextDelivery(ctx)
		if err != nil || d.Part != 1 || !w.Job.Acknowledged {
			t.Fatal("delivery progress lost", d, err)
		}
		if err = w.Delivered(ctx, 1, 43); err != nil {
			return err
		}
		return w.Complete(ctx)
	})
	if stage(t, s, "deliver", func(*Work) error { t.Fatal("completed job selected again"); return nil }) {
		t.Fatal("delivery repeated")
	}
}
func TestMigrationPreservesLegacyChatsAndIsRepeatable(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "CREATE TABLE chats(chat_id BIGINT PRIMARY KEY,title TEXT NOT NULL DEFAULT '',added_by BIGINT NOT NULL,added_at TIMESTAMPTZ NOT NULL DEFAULT now()); INSERT INTO chats(chat_id,added_by) VALUES(7,9)")
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dsn)
	s.Close()
	s = openStore(t, dsn)
	exists, err := s.Exists(ctx, 7)
	if err != nil || !exists {
		t.Fatal("legacy access lost", err)
	}
	var count int
	if err = conn.QueryRow(ctx, "SELECT COUNT(*) FROM bot_schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestSlowDDLHasSeparateBoundedMigrationBudget(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var schema string
	if err = admin.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	trigger := pgx.Identifier{schema + "_slow_ddl"}.Sanitize()
	// Delay only this test's migration pool; other tests may share the database.
	_, err = admin.Exec(ctx, `CREATE FUNCTION delay_ledger_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
  IF current_setting('application_name')='a03_slow_migration' AND position('bot_schema_migrations' IN current_query())>0 THEN
   PERFORM pg_sleep(6);
  END IF;
 END $$;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE EVENT TRIGGER "+trigger+" ON ddl_command_start WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION delay_ledger_ddl()"); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP EVENT TRIGGER "+trigger)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "a03_slow_migration"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrate(ctx, pool); err != nil {
		t.Fatalf("slow startup DDL incorrectly used ordinary query timeout: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM bot_schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatal("migration did not commit", count, err)
	}
	var timeout string
	if err = pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil || timeout != "5s" {
		t.Fatal("migration timeout leaked to ordinary queries", timeout, err)
	}
}
