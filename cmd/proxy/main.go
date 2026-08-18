// chatgpt-proxy — an MCP server that bridges AI agents to ChatGPT Web.
//
// Agents (Hermes, Claude Desktop, Codebuff-style assistants, ...) connect
// over MCP (stdio or Streamable HTTP) and use ask_chatgpt to send prompts
// to the user's ChatGPT Web session, driven through a dedicated Chrome
// profile via CDP — free, no API key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"chatgpt-proxy/internal/chatgpt"
	"chatgpt-proxy/internal/server"
)

func main() {
	transport := flag.String("transport", "stdio", "transport: stdio or http")
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (http transport)")
	chromePort := flag.Int("chrome-port", chatgpt.DefaultChromePort, "Chrome remote debugging port")
	profile := flag.String("profile", defaultProfile(), "Chrome profile directory (keeps the ChatGPT login)")
	chrome := flag.String("chrome", "", "Chrome binary path (auto-detected when empty)")
	open := flag.Bool("open", false, "just open Chrome with the profile and exit (first-time login)")
	waitLogin := flag.Bool("wait-login", false, "open Chrome and poll until the user is logged in at chatgpt.com, then exit")
	flag.Parse()

	cg := chatgpt.New(*chromePort, *profile, *chrome)
	defer cg.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *open {
		if err := openBrowser(cg); err != nil {
			log.Fatalf("open chrome: %v", err)
		}
		fmt.Println("Chrome opened with profile:", *profile)
		fmt.Println("Log in at chatgpt.com in that window once — the session is kept.")
		return
	}

	if *waitLogin {
		fmt.Println("Opening Chrome — please log in at chatgpt.com in the new window.")
		fmt.Println("The session is saved to", *profile, "and reused on later runs.")
		deadline := time.Now().Add(5 * time.Minute)
		for {
			st, err := cg.LoginState(ctx)
			if err != nil {
				log.Printf("login check: %v", err)
			} else if st.LoggedIn {
				fmt.Println("✅ Logged in — session saved.")
				return
			}
			if time.Now().After(deadline) {
				log.Fatal("timed out after 5 minutes waiting for login")
			}
			time.Sleep(3 * time.Second)
		}
	}

	log.SetOutput(os.Stderr)
	h := server.NewHandler(cg)

	switch *transport {
	case "stdio":
		log.Printf("chatgpt-proxy %s — MCP stdio server (chrome port %d, profile %s)", server.Version, *chromePort, *profile)
		if err := h.RunStdio(ctx); err != nil {
			log.Fatalf("stdio: %v", err)
		}
	case "http":
		log.Printf("chatgpt-proxy %s — MCP Streamable HTTP on http://%s/mcp (chrome port %d, profile %s)", server.Version, *addr, *chromePort, *profile)
		srv := &http.Server{Addr: *addr, Handler: h.NewHTTPHandler()}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	default:
		log.Fatalf("unknown transport %q (want stdio or http)", *transport)
	}
}

func defaultProfile() string {
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".chatgpt-proxy")
	}
	return ".chatgpt-proxy"
}

// openBrowser launches Chrome with the dedicated profile so the user can log
// in once. It reuses the same launch path as the chatgpt client.
func openBrowser(cg *chatgpt.Client) error {
	ctx := context.Background()
	// Status() attaches or launches Chrome and navigates to chatgpt.com.
	_, err := cg.Status(ctx)
	return err
}
