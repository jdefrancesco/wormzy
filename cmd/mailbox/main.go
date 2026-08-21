package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jdefrancesco/wormzy/internal/buildinfo"
	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	var (
		listen   = flag.String("listen", ":8080", "http listen address")
		redisURL = flag.String("redis", "127.0.0.1:6379", "redis connection url")
		ttl      = flag.Duration("ttl", 10*time.Minute, "session ttl")
		version  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Current().Format("mailbox"))
		return
	}

	server, err := transport.NewMailboxHTTPServer(*redisURL, *ttl)
	if err != nil {
		log.Fatalf("failed to init server: %v", err)
	}
	defer server.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	opsDone := make(chan struct{})
	go func() {
		defer close(opsDone)
		server.RunOperations(ctx, 5*time.Second)
	}()
	log.Printf("wormzy relay proxy listening on %s (redis %s)", *listen, *redisURL)
	srv := &http.Server{
		Addr:    *listen,
		Handler: server,
		// /v1/wait-peer and /v1/receive are long-poll style endpoints and can
		// legitimately hold the response open for most of the handshake window.
		// Keep read timeouts strict, but do not enforce a short write timeout.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("mailbox shutdown: %v", err)
		}
	}()
	err = srv.ListenAndServe()
	stop()
	<-opsDone
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
