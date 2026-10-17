package ipc_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestRegularHandlerContextCancelsWhenClientDisconnects(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)
	observed := make(chan struct{})
	srv.Handle("slow", func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		<-ctx.Done()
		close(observed)
		return nil, ctx.Err()
	})

	conn := rawDial(t, sock)
	req, err := ipc.NewRequest("slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("regular handler context was not canceled after client disconnect")
	}
}

func TestCallTimeoutClassifiesSlowReplySeparatelyFromConnectFailure(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)
	srv.Handle("slow", func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return map[string]string{"ok": "1"}, nil
		}
	})
	srv.Handle("fail", func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, errors.New("database unavailable")
	})

	slowClient, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	err = slowClient.CallWithTimeout("slow", struct{}{}, nil, 40*time.Millisecond)
	slowClient.Close()
	if err == nil {
		t.Fatal("expected a timeout from a slow live daemon")
	}
	if !ipc.IsCallTimeout(err) {
		t.Fatalf("slow reply error %T %v, want call timeout", err, err)
	}
	if ipc.IsConnectTimeout(err) {
		t.Fatalf("slow reply must not classify as a connect timeout: %v", err)
	}

	failClient, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer failClient.Close()
	err = failClient.CallWithTimeout("fail", struct{}{}, nil, time.Second)
	if err == nil {
		t.Fatal("expected RPC failure")
	}
	if ipc.IsCallTimeout(err) {
		t.Fatalf("RPC error classified as call timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("RPC error = %v, want database unavailable", err)
	}
}
