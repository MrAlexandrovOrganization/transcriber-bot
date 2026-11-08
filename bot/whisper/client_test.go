package whisper

import (
	"bytes"
	"context"
	"errors"
	"google.golang.org/grpc"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "transcriber-bot/gen/whisper"
)

// mockSendStream implements the interface expected by sendChunks.
type mockSendStream struct {
	chunks []*pb.TranscribeChunk
	err    error
}

func (m *mockSendStream) Send(c *pb.TranscribeChunk) error {
	if m.err != nil {
		return m.err
	}
	m.chunks = append(m.chunks, c)
	return nil
}

func newTestClient() *Client {
	return &Client{} // conn and stub not needed for unit tests
}

func TestSendChunks_SingleChunk(t *testing.T) {
	c := newTestClient()
	stream := &mockSendStream{}
	data := []byte("hello audio")

	if err := c.sendChunks(stream, bytes.NewReader(data), "ogg", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(stream.chunks))
	}
	if stream.chunks[0].Format != "ogg" {
		t.Errorf("format = %q, want ogg", stream.chunks[0].Format)
	}
	if string(stream.chunks[0].Data) != "hello audio" {
		t.Errorf("data mismatch")
	}
}

func TestSendChunks_MultipleChunks(t *testing.T) {
	c := newTestClient()
	stream := &mockSendStream{}

	// Create data larger than chunkSize (1MB) to force multiple chunks.
	data := make([]byte, chunkSize+512)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := c.sendChunks(stream, bytes.NewReader(data), "mp4", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(stream.chunks))
	}
	// Format is set only on the first chunk.
	if stream.chunks[0].Format != "mp4" {
		t.Errorf("first chunk format = %q, want mp4", stream.chunks[0].Format)
	}
	if stream.chunks[1].Format != "" {
		t.Errorf("subsequent chunk should have empty format, got %q", stream.chunks[1].Format)
	}
	// Total bytes match.
	total := 0
	for _, ch := range stream.chunks {
		total += len(ch.Data)
	}
	if total != len(data) {
		t.Errorf("total bytes = %d, want %d", total, len(data))
	}
}

func TestSendChunks_EmptyData(t *testing.T) {
	c := newTestClient()
	stream := &mockSendStream{}

	if err := c.sendChunks(stream, bytes.NewReader([]byte{}), "ogg", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stream.chunks) != 0 {
		t.Errorf("expected 0 chunks for empty data, got %d", len(stream.chunks))
	}
}

func TestSendChunks_SendError(t *testing.T) {
	c := newTestClient()
	sentinelErr := errors.New("network error")
	stream := &mockSendStream{err: sentinelErr}

	data := []byte("audio data")
	err := c.sendChunks(stream, bytes.NewReader(data), "ogg", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestWrapErr_Unavailable(t *testing.T) {
	c := newTestClient()
	grpcErr := status.Error(codes.Unavailable, "service down")

	err := c.wrapErr(grpcErr)

	var unavail *UnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected UnavailableError, got %T: %v", err, err)
	}
}

func TestWrapErr_DeadlineExceeded(t *testing.T) {
	c := newTestClient()
	grpcErr := status.Error(codes.DeadlineExceeded, "timeout")

	err := c.wrapErr(grpcErr)

	var unavail *UnavailableError
	if !errors.As(err, &unavail) {
		t.Errorf("expected UnavailableError for DeadlineExceeded, got %T: %v", err, err)
	}
}

func TestWrapErr_OtherError(t *testing.T) {
	c := newTestClient()
	grpcErr := status.Error(codes.InvalidArgument, "bad input")

	err := c.wrapErr(grpcErr)

	var unavail *UnavailableError
	if errors.As(err, &unavail) {
		t.Errorf("InvalidArgument should not wrap as UnavailableError")
	}
}

func TestWrapErr_NonGRPCError(t *testing.T) {
	c := newTestClient()
	plainErr := errors.New("plain error")

	err := c.wrapErr(plainErr)

	var unavail *UnavailableError
	if errors.As(err, &unavail) {
		t.Errorf("plain error should not wrap as UnavailableError")
	}
}

type earlyReplyBackend struct {
	pb.UnimplementedTranscriptionServiceServer
}

func (earlyReplyBackend) Submit(stream grpc.ClientStreamingServer[pb.TranscribeChunk, pb.SubmitResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.RequestId != "transcriber-bot:synthetic-replay" || !first.RetainUntilAck {
		return status.Error(codes.InvalidArgument, "missing durable identity")
	}
	// A replayed backend returns the persisted response before reading all audio.
	return stream.SendAndClose(&pb.SubmitResponse{JobId: "persisted-job"})
}
func TestDurableSubmitAcceptsEarlyReplayResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterTranscriptionServiceServer(server, earlyReplyBackend{})
	go server.Serve(listener)
	defer server.Stop()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	client, err := NewClient(host, port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	id, _, err := client.SubmitDurable(context.Background(), bytes.NewReader(make([]byte, 8*chunkSize)), "ogg", nil, "transcriber-bot:synthetic-replay")
	if err != nil || id != "persisted-job" {
		t.Fatalf("early replay response lost: id=%q err=%v", id, err)
	}
}

type availabilityBackend struct {
	pb.UnimplementedTranscriptionServiceServer
	deadlines chan time.Duration
}

func (s availabilityBackend) GetAvailability(ctx context.Context, _ *pb.AvailabilityRequest) (*pb.AvailabilityResponse, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "missing deadline")
	}
	s.deadlines <- time.Until(deadline)
	return &pb.AvailabilityResponse{Available: true}, nil
}
func TestAvailabilityHasShortDeadlineAndHonorsParent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	deadlines := make(chan time.Duration, 2)
	pb.RegisterTranscriptionServiceServer(server, availabilityBackend{deadlines: deadlines})
	go server.Serve(listener)
	defer server.Stop()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	client, err := NewClient(host, port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if available, err := client.Available(context.Background()); err != nil || !available {
		t.Fatal(available, err)
	}
	if remaining := <-deadlines; remaining <= 0 || remaining > 3*time.Second {
		t.Fatal("unbounded availability RPC", remaining)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if available, err := client.Available(ctx); err != nil || !available {
		t.Fatal(available, err)
	}
	if remaining := <-deadlines; remaining <= 0 || remaining > time.Second {
		t.Fatal("parent deadline ignored", remaining)
	}
}
