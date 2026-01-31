package sredird

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// -----------------------------------------------------------------------------
// Helper: PTY Management (Linux/Unix specific)
// -----------------------------------------------------------------------------

// openPTYPair creates a PTY controller/replica pair.
// It returns the ptmx file (controller) and the path to the pts device (replica).
func openPTYPair() (*os.File, string, error) {
	// 1. Open the PTY Multiplexer (Controller side)
	ptmxFile, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open /dev/ptmx: %w", err)
	}

	fd := int(ptmxFile.Fd())

	// 2. Unlock the PTS (Replica side)
	// TIOCSPTLCK expects a pointer to an int.
	// We use IoctlSetPointerInt to pass the address of '0'.
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		ptmxFile.Close()
		return nil, "", fmt.Errorf("unlockpt (TIOCSPTLCK) failed: %w", err)
	}

	// 3. Get the PTS path index
	// TIOCGPTN gets the pty number (IoctlGetInt handles the pointer internally)
	ptn, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		ptmxFile.Close()
		return nil, "", fmt.Errorf("failed to get pty number (TIOCGPTN): %w", err)
	}

	// Construct the secondary device path (e.g., /dev/pts/3)
	ptsPath := fmt.Sprintf("/dev/pts/%d", ptn)

	return ptmxFile, ptsPath, nil
}

// -----------------------------------------------------------------------------
// Test: End-to-End with /dev/ptmx
// -----------------------------------------------------------------------------

func TestEndToEnd_PTY(t *testing.T) {
	// 1. Environment Check
	if _, err := os.Stat("/dev/ptmx"); os.IsNotExist(err) {
		t.Skip("Skipping PTY test: /dev/ptmx not found")
	}

	// 2. PTY Setup
	ptmx, ptsPath, err := openPTYPair()
	if err != nil {
		t.Fatalf("Failed to create PTY pair: %v", err)
	}
	defer ptmx.Close()

	// CRITICAL FIX: Open the slave side in the test process too.
	// This ensures at least one FD is open on the slave, preventing the PTMX
	// from returning EIO (Input/Output Error) if sredird starts slowly or restarts.
	slaveKeeper, err := os.OpenFile(ptsPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Failed to open keep-alive handle to slave: %v", err)
	}
	defer slaveKeeper.Close()

	// 3. Network Pipe Setup
	serverNetRead, clientNetWrite := io.Pipe()
	clientNetRead, serverNetWrite := io.Pipe()

	// 4. SRedird Setup
	cfg := Config{
		PollInterval: 10,
		LogLevel:     0, // Set to 0 to keep test output clean, 1 for debugging
		Logger:       nil,
	}
	r := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Error channel to catch crashes in Run()
	errChan := make(chan error, 1)

	go func() {
		// Run connects to the ptsPath and bridges it to our pipes
		err := r.Run(ctx, serverNetRead, serverNetWrite, ptsPath)
		errChan <- err
	}()

	// Wait briefly for startup and verify Run() didn't exit immediately
	select {
	case err := <-errChan:
		t.Fatalf("sredird failed to start: %v", err)
	case <-time.After(100 * time.Millisecond):
		// SRedird is likely running fine
	}

	// --- Robust Data Expectation Helper ---
	expectData := func(t *testing.T, r io.Reader, expected []byte, desc string) {
		t.Helper()

		// Use a channel to stream data from the blocking reader
		dataStream := make(chan []byte)
		readErr := make(chan error, 1)

		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					// Send a copy of the data
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					select {
					case dataStream <- chunk:
					case <-ctx.Done():
						return
					}
				}
				if err != nil {
					readErr <- err
					return
				}
			}
		}()

		var acc []byte
		timeout := time.After(3 * time.Second)

		for {
			select {
			case chunk := <-dataStream:
				acc = append(acc, chunk...)
				if bytes.Contains(acc, expected) {
					return // Success!
				}
			case err := <-readErr:
				// If we hit an error but haven't found the data yet -> fail
				t.Fatalf("%s read error: %v. Buffer content: %q", desc, err, acc)
			case <-timeout:
				t.Fatalf("%s timed out. Got: %q, Want substring: %q", desc, acc, expected)
			}
		}
	}

	// 5. Test Case: Network -> Device (Write to Pipe, Read from PTMX)
	t.Run("NetworkToDevice", func(t *testing.T) {
		testMsg := []byte("Hello Serial Port")

		// Write asynchronously to prevent pipe blocking
		go func() {
			if _, err := clientNetWrite.Write(testMsg); err != nil {
				t.Logf("Write to clientNetWrite failed: %v", err)
			}
		}()

		expectData(t, ptmx, testMsg, "PTMX controller")
	})

	// 6. Test Case: Device -> Network (Write to PTMX, Read from Pipe)
	t.Run("DeviceToNetwork", func(t *testing.T) {
		testMsg := []byte("Hello Network")

		if _, err := ptmx.Write(testMsg); err != nil {
			t.Fatalf("Failed to write to PTMX: %v", err)
		}

		expectData(t, clientNetRead, testMsg, "Network pipe")
	})

	// Cleanup
	cancel()
	select {
	case <-errChan:
		// Normal exit
	case <-time.After(1 * time.Second):
		t.Error("Run() did not exit cleanly")
	}
}

// -----------------------------------------------------------------------------
// Unit Tests for Logic (Logic only, no PTY required)
// -----------------------------------------------------------------------------

func makeTestRedirector() *Redirector {
	cfg := Config{PollInterval: 0}
	r := New(cfg)
	r.deviceName = "MockDevice"
	r.deviceFd = 0
	return r
}

func TestTelnetState_NormalData(t *testing.T) {
	r := makeTestRedirector()
	input := []byte("Hello World")
	for _, b := range input {
		r.tnState.feed(r, b)
	}

	// Drain channel
	timeout := time.After(100 * time.Millisecond)
	var output []byte
	for i := 0; i < len(input); i++ {
		select {
		case b := <-r.devWriteChan:
			output = append(output, b...)
		case <-timeout:
			t.Fatal("Timeout waiting for device write channel")
		}
	}

	if !bytes.Equal(output, input) {
		t.Errorf("Expected %v, got %v", input, output)
	}
}

func TestTelnetState_IACEscaping(t *testing.T) {
	r := makeTestRedirector()
	// Send [255, 255] -> Expect [255]
	input := []byte{TNIAC, TNIAC}
	for _, b := range input {
		r.tnState.feed(r, b)
	}

	select {
	case data := <-r.devWriteChan:
		if len(data) != 1 || data[0] != TNIAC {
			t.Errorf("Expected [255], got %v", data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for escaped IAC")
	}
}

func TestRFC2217_Signature(t *testing.T) {
	r := makeTestRedirector()
	// IAC SB COM_PORT SIGNATURE IAC SE
	cmd := []byte{TNIAC, TNSB, TNCOM_PORT_OPTION, TNCAS_SIGNATURE, TNIAC, TNSE}
	for _, b := range cmd {
		r.tnState.feed(r, b)
	}

	select {
	case packet := <-r.netWriteChan:
		content := string(packet)
		if !strings.Contains(content, "SRedird") {
			t.Errorf("Signature response missing 'SRedird': %s", content)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for signature")
	}
}
