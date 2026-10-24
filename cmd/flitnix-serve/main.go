// Command flitnix-serve is a development helper: it starts the Flitnix
// phone-page server on a loopback address so the embedded webpage and routes
// can be smoke-tested locally with curl or a browser. It is not part of the
// desktop application build.
//
// Usage:
//
//	go run ./cmd/flitnix-serve [-addr 127.0.0.1:43821]
//
// Stop it with Ctrl+C (SIGINT) or by sending SIGTERM; it then drains
// gracefully and reports the stop.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/itsyamin/flitnix/internal/server"
)

func run() int {
	addr := flag.String("addr", server.DefaultListenAddr, "listen address as host:port (loopback only in this stage)")
	flag.Parse()

	srv := server.New(server.Config{ListenAddr: *addr})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serving := make(chan error, 1)
	go func() { serving <- srv.Serve(ctx) }()

	select {
	case err := <-serving:
		// Serve finished before becoming ready: a bind failure (e.g. port
		// occupied) or an immediate clean stop.
		if err != nil {
			fmt.Fprintf(os.Stderr, "flitnix-serve: %v\n", err)
			return 1
		}
		fmt.Println("flitnix-serve: stopped before becoming ready")
		return 0
	case <-srv.Ready():
		fmt.Printf("flitnix-serve: serving the phone page at %s (Ctrl+C to stop)\n", srv.URL())
		if err := <-serving; err != nil {
			fmt.Fprintf(os.Stderr, "flitnix-serve: %v\n", err)
			return 1
		}
		fmt.Println("flitnix-serve: stopped cleanly")
		return 0
	}
}

func main() {
	os.Exit(run())
}
