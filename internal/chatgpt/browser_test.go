package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	t.Run("pool workers never claim the shared tab file", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/json/list":
				fmt.Fprint(w, `[]`)
			case "/json/new":
				fmt.Fprint(w, `{"id":"worker-owned"}`)
			default:
				t.Errorf("unexpected ownership request %s", r.URL.Path)
			}
		}))
		defer srv.Close()
		profile := t.TempDir()
		c := New(srv.Listener.Addr().(*net.TCPAddr).Port, profile, "")
		c.actualPort = c.ChromePort
		c.DedicatedTab = true
		c.pool = &Pool{}
		id, err := c.findOrCreateTab()
		if err != nil || id != "worker-owned" || c.ownedTarget != id {
			t.Fatalf("worker did not create its own target: %s %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(profile, "openai-tab-id")); !os.IsNotExist(err) {
			t.Fatal("pool worker claimed the shared tab file")
		}
	})
	t.Run("owned clients never adopt another process target and close only their own", func(t *testing.T) {
		closed := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/json/list":
				fmt.Fprint(w, `[{"id":"other-process","type":"page","url":"https://chatgpt.com/c/live"}]`)
			case "/json/new":
				fmt.Fprint(w, `{"id":"owned-new"}`)
			case "/json/close/owned-new":
				closed <- "owned-new"
			default:
				t.Errorf("unexpected ownership request %s", r.URL.Path)
			}
		}))
		defer srv.Close()
		c := New(srv.Listener.Addr().(*net.TCPAddr).Port, t.TempDir(), "")
		c.actualPort = c.ChromePort
		c.DedicatedTab, c.SetupLocks = true, true
		if !c.UsesOwnedTabs() {
			t.Fatal("MCP/pooled owned client did not advertise owned-turn guard")
		}
		id, err := c.findOrCreateTab()
		if err != nil || id != "owned-new" || c.ownedTarget != id {
			t.Fatalf("adopted another process target: %s %v", id, err)
		}
		c.Close()
		if <-closed != "owned-new" {
			t.Fatal("owned shutdown did not close exactly its own target")
		}
		c.Close() // idempotent, must not close unrelated targets
	})
	t.Run("shared generations exclude legacy turns while setup remains separately serialized", func(t *testing.T) {
		profile := t.TempDir()
		a, err := LockOwnedTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer a()
		b, err := LockOwnedTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer b()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if _, err := LockTurn(ctx, profile); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("legacy whole-turn lock interleaved with owned generations: %v", err)
		}
		setup, err := LockSetup(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer setup()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel2()
		if _, err := LockSetup(ctx2, profile); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("project setup failed to serialize: %v", err)
		}
		setup()
		a()
		b()
		exclusive, err := LockTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		exclusive()
		state := filepath.Join(profile, "state.json")
		owner, err := LockState(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		defer owner()
		ctx3, cancel3 := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel3()
		if _, err := LockState(ctx3, state); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("two adapter writers admitted to same journal: %v", err)
		}
		if _, err := os.Stat(state + ".owner.lock"); err != nil {
			t.Fatal(err)
		}
	})

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
