package postgres

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestConnectToDatabase_UnresponsiveServerFailsWithinBound(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	c := &Client{config: Config{Host: "127.0.0.1", Port: addr.Port, Username: "u", Password: "p", SSLMode: "disable"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.connectToDatabase(ctx, "postgres")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a server that never answers")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("connectToDatabase did not return within 15s against an unresponsive server and a context without deadline")
	}
}
