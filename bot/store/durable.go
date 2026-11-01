package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql sql/migrations/*.sql
var statements embed.FS

func query(name string) string {
	data, err := statements.ReadFile("sql/" + name + ".sql")
	if err != nil {
		panic(err)
	}
	return string(data)
}

// Session locks also fence database writes: work always commits through the same
// connection that owns the lock. A lost connection cannot publish stale state.
func (s *Store) lock(ctx context.Context, key string) (*pgxpool.Conn, bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err = conn.QueryRow(ctx, query("lock_try"), key).Scan(&locked); err != nil || !locked {
		conn.Release()
		return nil, false, err
	}
	return conn, true, nil
}
func unlock(conn *pgxpool.Conn, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, query("lock_release"), key); err != nil {
		_ = conn.Hijack().Close(ctx)
	} else {
		conn.Release()
	}
}

type Poller struct {
	conn  *pgxpool.Conn
	key   string
	botID int64
}

func (s *Store) OpenPoller(ctx context.Context, botID int64) (*Poller, error) {
	key := fmt.Sprintf("transcriber-bot:poller:%d", botID)
	conn, ok, err := s.lock(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("another bot poller owns this database")
	}
	p := &Poller{conn: conn, key: key, botID: botID}
	if _, err = conn.Exec(ctx, query("cursor_init"), botID); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
func (p *Poller) Close() { unlock(p.conn, p.key) }
func (p *Poller) Cursor(ctx context.Context) (int, error) {
	var offset int
	err := p.conn.QueryRow(ctx, query("cursor_read"), p.botID).Scan(&offset)
	return offset, err
}

type Update struct {
	ID      int
	Payload json.RawMessage
}

// Accept atomically persists a complete getUpdates batch and its next offset.
func (p *Poller) Accept(ctx context.Context, updates []Update, nextOffset int) error {
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, update := range updates {
		if _, err = tx.Exec(ctx, query("inbox_insert"), p.botID, update.ID, update.Payload); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, query("cursor_write"), p.botID, nextOffset); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Job struct {
	Ready                                          bool
	BotID, UpdateID, ChatID, UserID                int64
	MessageID                                      int
	FileID, Format, Preset, Noun, RequestID, JobID string
	StatusMessageID                                int
	State                                          string
	Result                                         json.RawMessage
	Acknowledged, CancelRequested                  bool
	ProgressText                                   string
	Attempts                                       int
	NextAttempt                                    time.Time
}
type Delivery struct {
	Quoted     bool
	Part       int
	Kind, Text string
	MessageID  int
}

// ProcessInbox serializes admission (including preferences and membership) in
// update order. The job insert and inbox completion commit together.
func (s *Store) ProcessInbox(ctx context.Context, botID int64, accept func(int, json.RawMessage) (*Job, error)) (bool, error) {
	key := fmt.Sprintf("transcriber-bot:inbox:%d", botID)
	conn, ok, err := s.lock(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	defer unlock(conn, key)
	var id int
	var payload json.RawMessage
	err = conn.QueryRow(ctx, query("inbox_next"), botID).Scan(&id, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	job, err := accept(id, payload)
	if err != nil {
		return true, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	if job != nil {
		_, err = tx.Exec(ctx, query("job_insert"), botID, id, job.ChatID, job.UserID, job.MessageID, job.FileID, job.Format, job.Preset, job.Noun, fmt.Sprintf("transcriber-bot:%d:%d", botID, id))
		if err != nil {
			return true, err
		}
	}
	if _, err = tx.Exec(ctx, query("inbox_done"), botID, id); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}
func (s *Store) Preset(ctx context.Context, botID, userID int64) (string, error) {
	var preset string
	err := s.pool.QueryRow(ctx, query("preset_get"), botID, userID).Scan(&preset)
	return preset, err
}
func (s *Store) SetPreset(ctx context.Context, botID, userID int64, preset string) error {
	_, err := s.pool.Exec(ctx, query("preset_set"), botID, userID, preset)
	return err
}
func (s *Store) RequestCancel(ctx context.Context, botID int64, jobID string, chatID int64) error {
	_, err := s.pool.Exec(ctx, query("job_cancel"), botID, jobID, chatID)
	return err
}

type Work struct {
	Job  Job
	conn *pgxpool.Conn
}

func readJob(ctx context.Context, conn *pgxpool.Conn, botID, id int64) (Job, error) {
	var j Job
	err := conn.QueryRow(ctx, query("job_read"), botID, id).Scan(&j.BotID, &j.UpdateID, &j.ChatID, &j.UserID, &j.MessageID, &j.FileID, &j.Format, &j.Preset, &j.Noun, &j.RequestID, &j.JobID, &j.StatusMessageID, &j.State, &j.Result, &j.Acknowledged, &j.CancelRequested, &j.ProgressText, &j.Attempts, &j.NextAttempt, &j.Ready)
	return j, err
}

// WithWork claims one ready stage without holding a transaction across network
// calls. Uploads remain FIFO per chat; polling/delivery do not wait for old jobs.
func (s *Store) WithWork(ctx context.Context, botID int64, state string, run func(*Work) error) (bool, error) {
	rows, err := s.pool.Query(ctx, query("job_candidates"), botID, state)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		key := fmt.Sprintf("transcriber-bot:job:%d:%d", botID, id)
		conn, ok, err := s.lock(ctx, key)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		j, err := readJob(ctx, conn, botID, id)
		if err != nil {
			unlock(conn, key)
			return false, err
		}
		if j.State != state || !j.Ready {
			unlock(conn, key)
			continue
		}
		func() { defer unlock(conn, key); err = run(&Work{Job: j, conn: conn}) }()
		return true, err
	}
	return false, nil
}
func (w *Work) exec(ctx context.Context, name string, args ...any) error {
	values := append([]any{w.Job.BotID, w.Job.UpdateID}, args...)
	_, err := w.conn.Exec(ctx, query(name), values...)
	return err
}
func (w *Work) Status(ctx context.Context, id int) error { return w.exec(ctx, "job_status", id) }
func (w *Work) Submitted(ctx context.Context, id string) error {
	return w.exec(ctx, "job_submitted", id)
}
func (w *Work) Schedule(ctx context.Context, delay time.Duration, code string) error {
	return w.exec(ctx, "job_schedule", delay.Seconds(), code)
}
func (w *Work) Progress(ctx context.Context, text string) error {
	return w.exec(ctx, "job_progress", text)
}
func (w *Work) Acknowledged(ctx context.Context) error { return w.exec(ctx, "job_ack") }
func (w *Work) Complete(ctx context.Context) error     { return w.exec(ctx, "job_done") }
func (w *Work) Result(ctx context.Context, result json.RawMessage, parts []Delivery) error {
	if len(parts) == 0 || len(result) == 0 {
		return errors.New("terminal result requires a durable delivery plan")
	}
	tx, err := w.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, query("job_result"), w.Job.BotID, w.Job.UpdateID, result); err != nil {
		return err
	}
	for part, delivery := range parts {
		if _, err = tx.Exec(ctx, query("delivery_insert"), w.Job.BotID, w.Job.UpdateID, part, delivery.Kind, delivery.Text, delivery.Quoted); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (w *Work) NextDelivery(ctx context.Context) (*Delivery, error) {
	var d Delivery
	err := w.conn.QueryRow(ctx, query("delivery_next"), w.Job.BotID, w.Job.UpdateID).Scan(&d.Part, &d.Kind, &d.Text, &d.MessageID, &d.Quoted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &d, err
}
func (w *Work) Delivered(ctx context.Context, part, messageID int) error {
	return w.exec(ctx, "delivery_done", part, messageID)
}
