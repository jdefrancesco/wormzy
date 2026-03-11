package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/jdefrancesco/internal/transport"
)
wesome 
func main() {
	var (
		listen   = flag.String("listen", ":8080", "http listen address")
		redisURL = flag.String("redis", "127.0.0.1:6379", "redis connection url")
		ttl      = flag.Duration("ttl", time.Minute, "session ttl")
	)
	flag.Parse()

	server, err := transport.NewMailboxHTTPServer(*redisURL, *ttl)
	if err != nil {
		log.Fatalf("failed to init server: %v", err)
	}
	log.Printf("wormzy relay proxy listening on %s (redis %s)", *listen, *redisURL)
	if err := http.ListenAndServe(*listen, server); err != nil {
		log.Fatal(err)
	}
}
