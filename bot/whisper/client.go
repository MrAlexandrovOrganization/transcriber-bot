package whisper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
)

const (
	chunkSize     = 1 * 1024 * 1024 // 1 MB per chunk
	uploadTimeout = 120 * time.Second
	pollTimeout   = 10 * time.Second
)

// UnavailableError is returned when the Whisper service is unreachable.
type UnavailableError struct{ cause error }

func (e *UnavailableError) Unwrap() error { return e.cause }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("whisper service unavailable: %v", e.cause)
}

// Segment holds timing and text for a single transcribed segment.
type Segment struct {
	Start float32
	End   float32
	Text  string
}

// JobResult holds the outcome of an async transcription job.
type JobResult struct {
	Status          pb.JobStatus
	Stage           string
	Text            string
	Error           string
	Segments        []Segment
	ProgressPercent float32
	Attempts        int32
	MaxAttempts     int32
}

type Client struct {
	conn *grpc.ClientConn
	stub pb.TranscriptionServiceClient
}

func NewClient(host, port string) (*Client, error) {
	conn, err := grpc.NewClient(
		fmt.Sprintf("%s:%s", host, port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(50*1024*1024)),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}
	return &Client{conn: conn, stub: pb.NewTranscriptionServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Submit uploads audio and returns a job ID immediately.
// queuePosition indicates where in the queue this job is (1 = will run next).
// options may be nil to use server defaults.
func (c *Client) Submit(r io.Reader, format string, options *pb.TranscriptionOptions) (jobID string, queuePosition int, err error) {
	return c.SubmitContext(context.Background(), r, format, options)
}

// SubmitContext uploads audio while propagating the active W3C trace context.
func (c *Client) SubmitContext(parent context.Context, r io.Reader, format string, options *pb.TranscriptionOptions) (jobID string, queuePosition int, err error) {
	return c.submit(parent, r, format, options, "", false)
}

// SubmitDurable reuses an operation ID persisted before the first attempt.
func (c *Client) SubmitDurable(parent context.Context, r io.Reader, format string, options *pb.TranscriptionOptions, requestID string) (string, int, error) {
	if requestID == "" {
		return "", 0, fmt.Errorf("durable request ID required")
	}
	return c.submit(parent, r, format, options, requestID, true)
}
func (c *Client) submit(parent context.Context, r io.Reader, format string, options *pb.TranscriptionOptions, requestID string, retain bool) (string, int, error) {
	ctx, cancel := context.WithTimeout(outgoingContext(parent), uploadTimeout)
	defer cancel()

	stream, err := c.stub.Submit(ctx)
	if err != nil {
		return "", 0, c.wrapErr(err)
	}

	if err := c.sendChunksWithIdentity(stream, r, format, options, requestID, retain); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", 0, c.wrapErr(err)
	}
	return resp.JobId, int(resp.QueuePosition), nil
}

// Cancel requests cancellation of a job. Returns false if the job is already done.
func (c *Client) Cancel(jobID string) (bool, error) {
	return c.CancelContext(context.Background(), jobID)
}

// CancelContext requests cancellation while propagating the active W3C trace context.
func (c *Client) CancelContext(parent context.Context, jobID string) (bool, error) {
	ctx, cancel := context.WithTimeout(outgoingContext(parent), pollTimeout)
	defer cancel()

	resp, err := c.stub.Cancel(ctx, &pb.CancelRequest{JobId: jobID})
	if err != nil {
		return false, c.wrapErr(err)
	}
	return resp.Cancelled, nil
}

// GetStatus polls the status of a submitted job.
func (c *Client) GetStatus(jobID string) (*JobResult, error) {
	return c.GetStatusContext(context.Background(), jobID)
}

// GetStatusContext polls while propagating the active W3C trace context.
func (c *Client) GetStatusContext(parent context.Context, jobID string) (*JobResult, error) {
	ctx, cancel := context.WithTimeout(outgoingContext(parent), pollTimeout)
	defer cancel()

	resp, err := c.stub.GetStatus(ctx, &pb.StatusRequest{JobId: jobID})
	if err != nil {
		return nil, c.wrapErr(err)
	}

	segs := make([]Segment, len(resp.Segments))
	for i, s := range resp.Segments {
		segs[i] = Segment{Start: s.Start, End: s.End, Text: s.Text}
	}

	return &JobResult{
		Status:          resp.Status,
		Stage:           resp.Stage,
		Text:            resp.Text,
		Error:           resp.Error,
		Segments:        segs,
		ProgressPercent: resp.ProgressPercent,
		Attempts:        resp.Attempts,
		MaxAttempts:     resp.MaxAttempts,
	}, nil
}

// Ack is safe only after the result/error and delivery context commit locally.
func (c *Client) Ack(ctx context.Context, jobID string) error {
	ctx, cancel := context.WithTimeout(outgoingContext(ctx), pollTimeout)
	defer cancel()
	_, err := c.stub.Ack(ctx, &pb.AckRequest{JobId: jobID})
	if status.Code(err) == codes.NotFound {
		return nil
	} // Previous Ack may have succeeded before a crash.
	return c.wrapErr(err)
}

func outgoingContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	values := make([]string, 0, len(carrier)*2)
	for key, value := range carrier {
		values = append(values, key, value)
	}
	return metadata.AppendToOutgoingContext(ctx, values...)
}

// sendChunks sends audio data over a client-streaming RPC.
func (c *Client) sendChunks(stream interface {
	Send(*pb.TranscribeChunk) error
}, r io.Reader, format string, options *pb.TranscriptionOptions) error {
	return c.sendChunksWithIdentity(stream, r, format, options, "", false)
}
func (c *Client) sendChunksWithIdentity(stream interface {
	Send(*pb.TranscribeChunk) error
}, r io.Reader, format string, options *pb.TranscriptionOptions, requestID string, retain bool) error {
	buf := make([]byte, chunkSize)
	first := true
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := &pb.TranscribeChunk{Data: buf[:n]}
			if first {
				chunk.Format = format
				chunk.Options = options
				chunk.RequestId = requestID
				chunk.RetainUntilAck = retain
				first = false
			}
			if sendErr := stream.Send(chunk); sendErr != nil {
				return c.wrapErr(sendErr)
			}
		}
		if err == io.EOF {
			if first && requestID != "" {
				return stream.Send(&pb.TranscribeChunk{Format: format, Options: options, RequestId: requestID, RetainUntilAck: retain})
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read chunk: %w", err)
		}
	}
	return nil
}

func (c *Client) wrapErr(err error) error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
		return &UnavailableError{cause: err}
	}
	return err
}
