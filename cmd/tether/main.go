package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/guard"
	"tether/internal/mcp"
	"tether/internal/pty"
	"tether/internal/tui"
)

func main() {
	host := flag.String("host", "127.0.0.1", "Bind host address for the HTTP/SSE MCP server")
	port := flag.Int("port", 7432, "Fixed HTTP/SSE port for the tether server")
	ttl := flag.Duration("ttl", 30*time.Minute, "Access token TTL duration (e.g. 30m, 1h)")
	idleTimeout := flag.Duration("idle-timeout", 3*time.Second, "Duration without PTY output before reporting WAITING_FOR_INPUT")
	headless := flag.Bool("headless", false, "Run without interactive TUI (auto-approves requests; for testing only)")
	flag.Parse()

	listenAddr := fmt.Sprintf("%s:%d", *host, *port)
	baseURL := fmt.Sprintf("http://%s", listenAddr)

	sysInfo := env.Detect()
	eventBus := bus.NewEventBus()

	authMgr, err := auth.NewManager(eventBus, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tether: failed to initialize auth manager: %v\n", err)
		os.Exit(1)
	}

	cmdGuard := guard.NewGuard(eventBus)
	ptyMgr := pty.NewManager(sysInfo, eventBus, pty.WithIdleTimeout(*idleTimeout))
	defer ptyMgr.CloseAll()

	mcpSrv := mcp.NewServer(authMgr, cmdGuard, ptyMgr, eventBus, baseURL)
	defer mcpSrv.Close()

	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           mcpSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	if *headless {
		runHeadless(listenAddr, eventBus, httpSrv, serverErrCh)
		return
	}

	tuiModel := tui.NewModel(listenAddr, sysInfo, eventBus, authMgr, ptyMgr)
	defer tuiModel.Close()

	prog := tea.NewProgram(tuiModel, tea.WithAltScreen())
	go func() {
		if err := <-serverErrCh; err != nil {
			prog.Quit()
			fmt.Fprintf(os.Stderr, "tether HTTP server error on %s: %v\n", listenAddr, err)
		}
	}()

	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tether TUI error: %v\n", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func runHeadless(listenAddr string, eventBus *bus.EventBus, httpSrv *http.Server, serverErrCh <-chan error) {
	eventsCh, unsub := eventBus.Subscribe(64)
	defer unsub()

	go func() {
		for evt := range eventsCh {
			if evt.Type == bus.EventConsentQueued && evt.ConsentRequest != nil {
				_ = eventBus.ResolveConsent(evt.ConsentRequest.ID, bus.DecisionAllowOnce, "auto-approved in headless mode")
			}
		}
	}()

	fmt.Fprintf(os.Stdout, "tether running in headless mode on http://%s\n", listenAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigCh:
	case err := <-serverErrCh:
		fmt.Fprintf(os.Stderr, "tether HTTP server error: %v\n", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
