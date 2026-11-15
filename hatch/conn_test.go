package hatch

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func testStream() *Stream {
	closed := make(chan struct{})
	close(closed)
	c := &Conn{closed: closed, streams: map[int64]*Stream{}}
	s := &Stream{c: c, id: 1, notify: make(chan struct{}, 1)}
	c.streams[1] = s
	return s
}

func TestStreamCloseWakesReader(t *testing.T) {
	s := testStream()
	result := make(chan error, 1)
	go func() { _, err := s.NextJSON(context.Background()); result <- err }()
	s.Close()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader stranded by Close")
	}
}

func TestResetAfterHeadersIsNotSuccess(t *testing.T) {
	s := testStream()
	s.feed(&Frame{Response: &AppResponse{Status: 200, Body: []byte("partial")}})
	s.feed(&Frame{Reset: &Reset{Code: 1, Reason: "interrupted"}})
	if err := s.Wait(context.Background()); err == nil {
		t.Fatal("truncated response treated as success")
	}
}

func TestStreamReleasesConsumedBody(t *testing.T) {
	s := testStream()
	for i := 0; i < 1000; i++ {
		s.feed(&Frame{IsChunk: true, Chunk: []byte("{\"ok\":true}\n")})
		if _, err := s.NextJSON(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	retained := len(s.all) + len(s.pending)
	s.mu.Unlock()
	if retained > 1 {
		t.Fatalf("retained %d bytes after consuming stream", retained)
	}
	s.Close()
}

func TestCanceledOpenDoesNotSend(t *testing.T) {
	c := &Conn{streams: map[int64]*Stream{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.OpenContext(ctx, Request{Method: "POST", Path: "/charged"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(c.streams) != 0 {
		t.Fatal("canceled request registered a stream")
	}
}

func TestMalformedAssembliesDoNotAccumulate(t *testing.T) {
	var r Reassembler
	for i := int64(1); i <= 100; i++ {
		first := pbuf{}.uint(1, uint64(i)).uint(2, 0).uint(3, 2).bytes(4, []byte("part"))
		if _, err := r.Feed(first); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Feed(first); err == nil {
			t.Fatal("duplicate chunk accepted")
		}
	}
	if len(r.pending) != 0 || len(r.order) != 0 || r.buffered != 0 {
		t.Fatal("malformed chunks retained bookkeeping or payload")
	}
}

func TestNextJSONAcrossChunks(t *testing.T) {
	s := testStream()
	record := `{"a":"x}\"y","b":[1,{"c":2}]}`
	go func() {
		s.feed(&Frame{IsChunk: true, Chunk: []byte("  \n")})
		for i := 0; i < len(record); i += 3 {
			s.feed(&Frame{IsChunk: true, Chunk: []byte(record[i:min(i+3, len(record))])})
		}
		s.feed(&Frame{IsChunk: true, Chunk: []byte("\ndata: junk\n[1,2]"), ChunkEnd: true})
	}()
	for _, want := range []string{record, "[1,2]"} {
		got, err := s.NextJSON(context.Background())
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	if _, err := s.NextJSON(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
}

func TestNextChunkStreamsBody(t *testing.T) {
	s := testStream()
	go func() {
		s.feed(&Frame{Response: &AppResponse{Status: 200, Body: []byte("ab")}})
		s.feed(&Frame{IsChunk: true, Chunk: []byte("cd")})
		s.feed(&Frame{IsChunk: true, Chunk: []byte("ef"), ChunkEnd: true})
	}()
	var got []byte
	for {
		b, err := s.NextChunk(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b...)
	}
	if string(got) != "abcdef" || s.Status() != 200 {
		t.Fatalf("got %q status %d", got, s.Status())
	}
}

func TestNextChunkAcceptsValidMediaBurstWhileConsumerIsPaused(t *testing.T) {
	s := testStream()
	s.feed(&Frame{Response: &AppResponse{Status: 200, Body: []byte("head")}})
	if _, err := s.NextChunk(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A valid 65 MiB video arrives while its consumer is waiting on disk. This
	// must retain the existing 512 MiB response allowance, not fail at 64 MiB.
	chunk := make([]byte, 1<<20)
	for i := 0; i < 65; i++ {
		s.feed(&Frame{IsChunk: true, Chunk: chunk, ChunkEnd: i == 64})
	}
	total := 0
	for {
		body, err := s.NextChunk(context.Background())
		total += len(body)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("valid media burst rejected after %d bytes: %v", total, err)
		}
	}
	if total != 65<<20 {
		t.Fatalf("truncated media: got %d bytes", total)
	}
}

func TestOpenContextPreSendReturnsNotSentError(t *testing.T) {
	closed := make(chan struct{})
	close(closed)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Conn{closed: closed, writeGate: make(chan struct{}, 1), streams: map[int64]*Stream{}}
	if _, err := c.OpenContext(canceled, Request{Method: "GET", Path: "/ping"}); err == nil {
		t.Fatal("expected error for canceled context")
	} else {
		var ns *NotSentError
		if !errors.As(err, &ns) || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected NotSentError(context.Canceled), got %T: %v", err, err)
		}
	}

	if _, err := c.OpenContext(context.Background(), Request{Method: "GET", Path: "/ping"}); err == nil {
		t.Fatal("expected error for closed conn")
	} else {
		var ns *NotSentError
		if !errors.As(err, &ns) || !errors.Is(err, ErrClosed) {
			t.Fatalf("expected NotSentError(ErrClosed), got %T: %v", err, err)
		}
	}

	cErr := &Conn{err: ErrClosed, closed: closed, writeGate: make(chan struct{}, 1), streams: map[int64]*Stream{}}
	if _, err := cErr.OpenContext(context.Background(), Request{Method: "GET", Path: "/ping"}); err == nil {
		t.Fatal("expected error for failed conn")
	} else {
		var ns *NotSentError
		if !errors.As(err, &ns) || !errors.Is(err, ErrClosed) {
			t.Fatalf("expected NotSentError(ErrClosed), got %T: %v", err, err)
		}
	}
}
