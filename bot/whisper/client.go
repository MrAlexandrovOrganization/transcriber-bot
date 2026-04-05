package whisper

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("whisper service unavailable: %v", e.cause)
}

// JobResult holds the outcome of an async transcription job.
type JobResult struct {
	Status pb.JobStatus
	Text   string
	Error  string
}

type Client struct {
	conn *grpc.ClientConn
	stub pb.TranscriptionServiceClient
}

func NewClient(host, port string) (*Client, error) {
	conn, err := grpc.NewClient(
		fmt.Sprintf("%s:%s", host, port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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
func (c *Client) Submit(r io.Reader, format string) (jobID string, queuePosition int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	stream, err := c.stub.Submit(ctx)
	if err != nil {
		return "", 0, c.wrapErr(err)
	}

	if err := c.sendChunks(stream, r, format); err != nil {
		return "", 0, err
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", 0, c.wrapErr(err)
	}
	return resp.JobId, int(resp.QueuePosition), nil
}

// GetStatus polls the status of a submitted job.
func (c *Client) GetStatus(jobID string) (*JobResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	resp, err := c.stub.GetStatus(ctx, &pb.StatusRequest{JobId: jobID})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	return &JobResult{
		Status: resp.Status,
		Text:   resp.Text,
		Error:  resp.Error,
	}, nil
}

// sendChunks sends audio data over a client-streaming RPC.
func (c *Client) sendChunks(stream interface {
	Send(*pb.TranscribeChunk) error
}, r io.Reader, format string) error {
	buf := make([]byte, chunkSize)
	first := true
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := &pb.TranscribeChunk{Data: buf[:n]}
			if first {
				chunk.Format = format
				first = false
			}
			if sendErr := stream.Send(chunk); sendErr != nil {
				return c.wrapErr(sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read chunk: %w", err)
		}
	}
	return nil
}

func (c *Client) wrapErr(err error) error {
	st, _ := status.FromError(err)
	if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
		return &UnavailableError{cause: err}
	}
	return err
}
