package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jdefrancesco/internal/rendezvous"
)

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	tlsCert := flag.String("tlscert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tlskey", "", "TLS private key file (PEM)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv := &rendezvous.Server{
		Addr:        *addr,
		TLSCertFile: *tlsCert,
		TLSKeyFile:  *tlsKey,
		Logger:      logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("rendezvous server stopped", "err", err)
		os.Exit(1)
	}

	logger.Info("rendezvous server stopped")
}
