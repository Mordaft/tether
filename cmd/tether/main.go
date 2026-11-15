package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/guard"
)

func main() {
	port := flag.Int("port", 7432, "Fixed HTTP/SSE port for tether server")
	ttl := flag.Duration("ttl", 30*time.Minute, "Access token TTL duration (e.g. 30m, 1h)")
	flag.Parse()

	eventBus := bus.NewEventBus()
	authMgr, err := auth.NewManager(eventBus, *ttl)
	if err != nil {
		log.Fatalf("failed to initialize auth manager: %v", err)
	}
	cmdGuard := guard.NewGuard(eventBus)

	_ = authMgr
	_ = cmdGuard

	fmt.Fprintf(os.Stdout, "tether initialized on 127.0.0.1:%d (token TTL: %s)\n", *port, ttl.String())
}
