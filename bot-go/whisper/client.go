package whisper

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
)

const (
	chunkSize = 1 * 1024 * 1024 // 1 MB
	timeout   = 120 * time.Second
)

type UnavailableError struct{ cause error }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("whisper service unavailable: %v", e.cause)
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

func (c *Client) Transcribe(audioData []byte, format string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stream, err := c.stub.Transcribe(ctx)
	if err != nil {
		return "", c.wrapErr(err)
	}

	for i := 0; i < len(audioData); i += chunkSize {
		end := min(i+chunkSize, len(audioData))
		chunk := &pb.TranscribeChunk{Data: audioData[i:end]}
		if i == 0 {
			chunk.Format = format
		}
		if err := stream.Send(chunk); err != nil {
			return "", c.wrapErr(err)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", c.wrapErr(err)
	}
	return resp.Text, nil
}

func (c *Client) wrapErr(err error) error {
	st, _ := status.FromError(err)
	if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
		return &UnavailableError{cause: err}
	}
	return err
}
