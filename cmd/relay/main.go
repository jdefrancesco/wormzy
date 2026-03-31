package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	listen := flag.String("listen", ":3478", "UDP listen address for the QUIC relay")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := &transport.RelayServer{Addr: *listen}
	if err := server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("relay server error: %v", err)
	}
}
