package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	listen := flag.String("listen", ":3478", "UDP listen address for the QUIC relay")
	redisURL := flag.String("redis", defaultTelemetryRedisURL(), "redis URL for operator telemetry and controls")
	prefix := flag.String("prefix", "wormzy", "redis key prefix")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	telemetry, err := transport.NewServiceTelemetry(*redisURL, *prefix, "relay")
	if err != nil {
		log.Fatalf("relay telemetry: %v", err)
	}
	if telemetry == nil {
		log.Printf("relay telemetry disabled: configure -redis or WORMZY_RELAY_REDIS")
	} else {
		defer telemetry.Close()
	}
	opsDone := make(chan struct{})
	go func() {
		defer close(opsDone)
		telemetry.Run(ctx, 5*time.Second)
	}()

	server := &transport.RelayServer{Addr: *listen, Telemetry: telemetry}
	err = server.ListenAndServe(ctx)
	stopping := ctx.Err() != nil
	stop()
	<-opsDone
	if err != nil && !stopping {
		log.Fatalf("relay server error: %v", err)
	}
}

func defaultTelemetryRedisURL() string {
	for _, name := range []string{"WORMZY_RELAY_REDIS", "WORMZY_METRICS_REDIS", "WORMZY_MAILBOX_REDIS"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
