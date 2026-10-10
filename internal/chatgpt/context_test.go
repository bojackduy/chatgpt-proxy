package chatgpt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSameURL(t *testing.T) {
	same := [][2]string{
		{"https://chatgpt.com/c/abc", "https://chatgpt.com/c/abc"},
		{"https://chatgpt.com/c/abc/", "https://chatgpt.com/c/abc"},
		{"https://chatgpt.com/c/abc#frag", "https://chatgpt.com/c/abc"},
	}
	for _, s := range same {
		if !sameURL(s[0], s[1]) {
			t.Fatalf("expected same: %q %q", s[0], s[1])
		}
	}
	diff := [][2]string{
		{"https://chatgpt.com/c/abc", "https://chatgpt.com/c/def"},
		{"https://chatgpt.com/", "https://chatgpt.com/c/abc"},
		{"", "https://chatgpt.com/c/abc"},
	}
	for _, s := range diff {
		if sameURL(s[0], s[1]) {
			t.Fatalf("expected different: %q %q", s[0], s[1])
		}
	}
}

func TestRequestContextCancellationDoesNotCancelTab(t *testing.T) {
	key := struct{}{}
	tab := context.WithValue(context.Background(), key, "CDP executor")
	request, cancel := context.WithCancel(context.Background())
	child, done := requestContext(request, tab)
	defer done()
	if child.Value(key) != "CDP executor" {
		t.Fatal("lost tab values")
	}
	cancel()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not propagate")
	}
	if tab.Err() != nil {
		t.Fatal("canceled persistent browser context")
	}
}

func TestMCPDoesNotAdoptDedicatedAdapterTab(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			fmt.Fprint(w, `[{"id":"adapter","type":"page","url":"https://chatgpt.com/c/adapter"},{"id":"mcp","type":"page","url":"https://chatgpt.com/c/mcp"}]`)
			return
		}
		if r.Method != "PUT" || r.URL.Path != "/json/new" {
			t.Error("unexpected request", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"id":"new-adapter"}`)
	}))
	defer server.Close()
	port, _ := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "openai-tab-id"), []byte("adapter"), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(port, profile, "")
	id, err := c.findOrCreateTab()
	if err != nil || string(id) != "mcp" {
		t.Fatal("MCP adopted adapter tab", id, err)
	}
	c.DedicatedTab = true
	id, err = c.findOrCreateTab()
	if err != nil || string(id) != "new-adapter" {
		t.Fatal("adapter reused MCP tab", id, err)
	}
	b, err := os.ReadFile(filepath.Join(profile, "openai-tab-id"))
	if err != nil || string(b) != "new-adapter" {
		t.Fatal("private target not registered", err)
	}
}
