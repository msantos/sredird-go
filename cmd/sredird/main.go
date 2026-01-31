package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.iscode.ca/sredird/pkg/sredird"
)

func main() {
	var (
		timeoutSec   int
		pollInterval int
		logLevel     int
		showHelp     bool
		ciscoCompat  bool
	)

	// Define flags replicating the original options
	flag.IntVar(&timeoutSec, "t", 0, "Set inactivity timeout in seconds")
	flag.BoolVar(&ciscoCompat, "i", false, "Indicates Cisco IOS Bug compatibility")
	flag.BoolVar(&ciscoCompat, "cisco-compatibility", false, "Indicates Cisco IOS Bug compatibility") // Long alias
	flag.BoolVar(&showHelp, "h", false, "Show help")
	flag.BoolVar(&showHelp, "help", false, "Show help")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sredird: RFC 2217 compliant serial port redirector\n%s\n", sredird.SRedirdVersionId)
		fmt.Fprintf(os.Stderr, "Usage: sredird [options] <loglevel> <device> [pollinginterval]\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "Poll interval is in milliseconds, default is 100, 0 means no polling\n")
	}

	flag.Parse()

	if showHelp {
		flag.Usage()
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) < 2 {
		flag.Usage()
		os.Exit(1)
	}

	// Parse Positional Args
	fmt.Sscanf(args[0], "%d", &logLevel)
	deviceName := args[1]
	pollInterval = 100
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%d", &pollInterval)
	}

	// Initialize Configuration
	config := sredird.Config{
		Timeout:      timeoutSec,
		PollInterval: pollInterval,
		LogLevel:     logLevel,
		CiscoCompat:  ciscoCompat,
	}

	// Create Redirector
	redirector := sredird.New(config)

	// Setup Context with Cancellation on Signal
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer cancel()

	// Run the redirector
	// This blocks until context is cancelled or an error occurs
	err := redirector.Run(ctx, os.Stdin, os.Stdout, deviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
