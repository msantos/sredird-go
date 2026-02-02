package sredird_test

import (
	"context"
	"io"
	"log"
	"net"

	"go.iscode.ca/sredird/pkg/sredird"
)

// ExampleNew_session demonstrates the standard lifecycle of a redirection session.
// It highlights that a new Redirector must be instantiated for every connection
// to ensure Telnet state and device file descriptors are managed safely.
func ExampleNew() {
	// 1. Define configuration
	cfg := sredird.Config{
		Timeout:      300, // 5-minute inactivity timeout
		PollInterval: 100, // Poll modem lines every 100ms
		LogLevel:     1,
	}

	// 2. Create a NEW instance for this specific session.
	// WARNING: Do not share or reuse this instance across multiple goroutines.
	rd := sredird.New(cfg)

	// 3. Setup context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 4. In a real scenario, 'conn' would be a net.Conn from ln.Accept()
	// and 'device' would be a path like "/dev/ttyS0".
	go func() {
		// Mocking network and device for demonstration
		networkReader, networkWriter := io.Pipe()

		err := rd.Run(ctx, networkReader, networkWriter, "/dev/ttyS0")
		if err != nil && err != context.Canceled {
			log.Printf("Session exited with error: %v", err)
		}
	}()

	// Trigger shutdown after some logic
	cancel()
}

// ExampleRedirector_Run_concurrency demonstrates how to handle multiple
// serial ports simultaneously by creating unique Redirector instances.
func ExampleRedirector_Run_concurrency() {
	ports := []string{"/dev/ttyS0", "/dev/ttyS1"}

	for _, port := range ports {
		// Every port (and every new connection to a port)
		// must have its own isolated Redirector.
		rd := sredird.New(sredird.Config{Timeout: 60})

		go func(p string, r *sredird.Redirector) {
			// Create a dummy listener to simulate a network source
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			conn, _ := ln.Accept()
			defer conn.Close()

			// Run blocks until the connection drops or context is cancelled
			r.Run(context.Background(), conn, conn, p)
		}(port, rd)
	}
}

// ExampleConfig_ciscoCompat shows how to initialize the redirector
// with specific RFC 2217 compatibility flags.
func ExampleConfig_ciscoCompat() {
	cfg := sredird.Config{
		CiscoCompat: true, // Enables specific IOS bug workarounds
		Logger:      log.Default(),
	}

	rd := sredird.New(cfg)
	_ = rd // Use in rd.Run(...)
}
