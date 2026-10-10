package chatgpt

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func versionServer(t *testing.T, uuid string) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Browser":"Test","webSocketDebuggerUrl":%q}`, uuid)
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return srv, port
}

func TestBrowserAlive(t *testing.T) {
	const uuid = "ws://127.0.0.1:9222/devtools/browser/abc-123"
	_, port := versionServer(t, uuid)
	c := New(0, t.TempDir(), "")
	c.actualPort = port
	if c.browserAlive() {
		t.Fatal("empty wsURL must report stale")
	}
	c.wsURL = uuid
	if !c.browserAlive() {
		t.Fatal("matching endpoint must report alive")
	}
	c.wsURL = "ws://127.0.0.1:9222/devtools/browser/rotated-456"
	if c.browserAlive() {
		t.Fatal("rotated endpoint (Chrome restart) must report stale")
	}
	// Dead listener must report stale, never hang: bind then close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.actualPort = dead
	if c.browserAlive() {
		t.Fatal("unreachable port must report stale")
	}
}
