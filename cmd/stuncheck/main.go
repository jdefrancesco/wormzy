// STUNCheck is a simple tool to check if I am using it correctly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/jdefrancesco/wormzy/internal/buildinfo"
	"github.com/jdefrancesco/wormzy/internal/stun"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Current().Format("stuncheck"))
		return
	}

	// simple logger to stdout
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter{}, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := stun.NewStun()
	if s.IpV4Addr != nil {
		fmt.Println("Public UDP address:", s.IpV4Addr)
		return
	}
	// try discovery explicitly with timeout
	_ = ctx
	// fallback: run discoverIPv4 directly (not exported) - call NewStun above is best-effort
	fmt.Println("No STUN address discovered")
}

type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	return fmt.Print(string(p))
}
