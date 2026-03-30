package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/jdefrancesco/wormzy/internal/transport"
)

func main() {
	var (
		listen   = flag.String("listen", ":8080", "http listen address")
		redisURL = flag.String("redis", "127.0.0.1:6379", "redis connection url")
		ttl      = flag.Duration("ttl", 10*time.Minute, "session ttl")
	)
	flag.Parse()

	server, err := transport.NewMailboxHTTPServer(*redisURL, *ttl)
	if err != nil {
		log.Fatalf("failed to init server: %v", err)
	}
	log.Printf("wormzy relay proxy listening on %s (redis %s)", *listen, *redisURL)
	srv := &http.Server{
		Addr:         *listen,
		Handler:      server,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
