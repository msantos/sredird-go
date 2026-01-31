package sredird

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// -----------------------------------------------------------------------------
// Constants (Telnet & RFC 2217)
// -----------------------------------------------------------------------------

const (
	VersionId        = "2.2.1-go"
	SRedirdVersionId = "Version " + VersionId + ", Go Port"
	BufferSize       = 2048

	// Telnet Protocol Constants
	TNSE   = 240
	TNNOP  = 241
	TNSB   = 250
	TNWILL = 251
	TNWONT = 252
	TNDO   = 253
	TNDONT = 254
	TNIAC  = 255

	// Telnet Options
	TN_TRANSMIT_BINARY   = 0
	TN_ECHO              = 1
	TN_SUPPRESS_GO_AHEAD = 3
	TNCOM_PORT_OPTION    = 44

	// RFC 2217 Client to Server
	TNCAS_SIGNATURE           = 0
	TNCAS_SET_BAUDRATE        = 1
	TNCAS_SET_DATASIZE        = 2
	TNCAS_SET_PARITY          = 3
	TNCAS_SET_STOPSIZE        = 4
	TNCAS_SET_CONTROL         = 5
	TNCAS_NOTIFY_LINESTATE    = 6
	TNCAS_NOTIFY_MODEMSTATE   = 7
	TNCAS_FLOWCONTROL_SUSPEND = 8
	TNCAS_FLOWCONTROL_RESUME  = 9
	TNCAS_SET_LINESTATE_MASK  = 10
	TNCAS_SET_MODEMSTATE_MASK = 11
	TNCAS_PURGE_DATA          = 12

	// RFC 2217 Server to Client
	TNASC_SIGNATURE           = 100
	TNASC_SET_BAUDRATE        = 101
	TNASC_SET_DATASIZE        = 102
	TNASC_SET_PARITY          = 103
	TNASC_SET_STOPSIZE        = 104
	TNASC_SET_CONTROL         = 105
	TNASC_NOTIFY_LINESTATE    = 106
	TNASC_NOTIFY_MODEMSTATE   = 107
	TNASC_FLOWCONTROL_SUSPEND = 108
	TNASC_FLOWCONTROL_RESUME  = 109
	TNASC_SET_LINESTATE_MASK  = 110
	TNASC_SET_MODEMSTATE_MASK = 111
	TNASC_PURGE_DATA          = 112
)

// Config holds the configuration for the SRedird session.
type Config struct {
	// Timeout is the inactivity timeout in seconds. 0 means no timeout.
	Timeout int
	// PollInterval is the modem line polling interval in milliseconds.
	PollInterval int
	// LogLevel controls the verbosity of logs (logic preserved from original, though not deeply used).
	LogLevel int
	// CiscoCompat enables Cisco IOS Bug compatibility mode.
	CiscoCompat bool
	// Logger is an optional logger. If nil, log.Default() is used.
	Logger *log.Logger
}

// Redirector represents a running serial redirection session.
// It encapsulates the state formerly held in global variables.
type Redirector struct {
	cfg Config
	log *log.Logger

	// State
	deviceName          string
	deviceFd            int
	tcpcEnabled         bool
	initialPortSettings *unix.Termios
	modemStateMask      uint8
	lineStateMask       uint8
	breakSignaled       bool
	inputFlow           bool
	lastModemState      uint8

	// Channels
	activityChan chan struct{}
	netWriteChan chan []byte
	devWriteChan chan []byte
	errChan      chan error

	// Concurrency control
	mu sync.Mutex

	// Telnet State
	tnState *telnetStateMachine
}

// New creates a new Redirector with the provided configuration.
func New(cfg Config) *Redirector {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	return &Redirector{
		cfg:            cfg,
		log:            logger,
		deviceFd:       -1,
		modemStateMask: 255,
		lineStateMask:  0,
		inputFlow:      true,
		activityChan:   make(chan struct{}, 1),
		netWriteChan:   make(chan []byte, BufferSize),
		devWriteChan:   make(chan []byte, BufferSize),
		errChan:        make(chan error, 1),
		tnState:        newTelnetStateMachine(),
	}
}

// Run starts the redirection process.
// It opens the serial device specified by deviceName and bridges it with netReader and netWriter.
// Run blocks until the context is cancelled, a fatal error occurs, or the inactivity timeout is reached.
func (r *Redirector) Run(ctx context.Context, netReader io.Reader, netWriter io.Writer, deviceName string) error {
	r.deviceName = deviceName
	r.log.SetPrefix(deviceName + ": ")
	r.logf("SRedird started. LogLevel: %d, Device: %s", r.cfg.LogLevel, deviceName)

	// 1. Open Device
	fd, err := unix.Open(deviceName, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("unable to open device %s: %w", deviceName, err)
	}
	r.deviceFd = fd

	// 2. Save initial settings
	r.initialPortSettings, err = getTermios(r.deviceFd)
	if err != nil {
		unix.Close(r.deviceFd)
		return fmt.Errorf("tcgetattr failed: %w", err)
	}

	// 3. Setup cleanup
	defer func() {
		r.restoreSettings()
		if err := unix.Close(r.deviceFd); err != nil {
			r.logf("closed: %d: %v", r.deviceFd, err)
		}
		r.logf("SRedird stopped.")
	}()

	// 4. Setup Port
	if err := r.setupSerialPort(r.deviceFd); err != nil {
		return err
	}

	// 5. Start Goroutines
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // Ensure child goroutines stop if Run returns

	// Network Reader
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.networkReader(ctx, netReader)
	}()

	// Network Writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.networkWriter(ctx, netWriter)
	}()

	// Device Reader
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.deviceReader(ctx)
	}()

	// Device Writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.deviceWriter(ctx)
	}()

	// Modem Poller
	if r.cfg.PollInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.modemPoller(ctx, time.Duration(r.cfg.PollInterval)*time.Millisecond)
		}()
	}

	// 6. Initial Telnet Negotiation
	r.sendInitialNegotiation()

	// 7. Timeout Timer Logic
	timer := time.NewTimer(100000 * time.Hour) // Dummy duration
	if r.cfg.Timeout > 0 {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Duration(r.cfg.Timeout) * time.Second)
	} else {
		timer.Stop()
	}
	defer timer.Stop()

	// 8. Main Loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-r.errChan:
			// If EOF, it's a "clean" shutdown request, else it's an error
			if err == io.EOF {
				return nil
			}
			return err
		case <-timer.C:
			if r.cfg.Timeout > 0 {
				r.logf("Inactivity timeout.")
				return nil
			}
		case <-r.activityChan:
			if r.cfg.Timeout > 0 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Duration(r.cfg.Timeout) * time.Second)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Core Logic (formerly Goroutines)
// -----------------------------------------------------------------------------

func (r *Redirector) networkReader(ctx context.Context, reader io.Reader) {
	buf := make([]byte, BufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Note: Read is blocking. Context cancellation might not interrupt this immediately
			// unless the underlying Reader supports it or is closed externally.
			n, err := reader.Read(buf)
			if n > 0 {
				r.signalActivity()
				for i := 0; i < n; i++ {
					r.tnState.feed(r, buf[i])
				}
			}
			if err != nil {
				if err != io.EOF {
					r.sendError(fmt.Errorf("error reading from network: %w", err))
				} else {
					r.sendError(io.EOF) // Signal clean exit
				}
				return
			}
		}
	}
}

func (r *Redirector) networkWriter(ctx context.Context, writer io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-r.netWriteChan:
			r.signalActivity()
			_, err := writer.Write(data)
			if err != nil {
				r.sendError(fmt.Errorf("error writing to network: %w", err))
				return
			}
		}
	}
}

func (r *Redirector) deviceReader(ctx context.Context) {
	buf := make([]byte, BufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if !r.inputFlow {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			fds := []unix.PollFd{
				{
					Fd:     int32(r.deviceFd),
					Events: unix.POLLIN, // Watch for input events
				},
			}

			if n, err := unix.Poll(fds, r.cfg.PollInterval); err != nil {
				if err == unix.EINTR {
					continue
				}
				r.sendError(fmt.Errorf("error polling device: %w", err))
				return
			} else if n == 0 {
				continue // timeout
			}

			n, err := unix.Read(r.deviceFd, buf)
			if n > 0 {
				r.signalActivity()
				var out []byte
				for i := 0; i < n; i++ {
					c := buf[i]
					if c == TNIAC {
						out = append(out, TNIAC, TNIAC)
					} else {
						out = append(out, c)
					}
				}
				select {
				case r.netWriteChan <- out:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
					continue
				}
				r.sendError(fmt.Errorf("error reading from device: %w", err))
				return
			}
		}
	}
}

func (r *Redirector) deviceWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-r.devWriteChan:
			r.signalActivity()
			_, err := unix.Write(r.deviceFd, data)
			if err != nil {
				r.sendError(fmt.Errorf("error writing to device: %w", err))
				return
			}
		}
	}
}

func (r *Redirector) modemPoller(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.tcpcEnabled && r.inputFlow {
				currentState := getModemState(r.deviceFd, r.lastModemState)
				if (currentState & r.modemStateMask) != (r.lastModemState & r.modemStateMask) {
					r.lastModemState = currentState
					r.sendCPCByteCommand(TNASC_NOTIFY_MODEMSTATE, r.lastModemState&r.modemStateMask)
					r.logf("Sent modem state: %d", r.lastModemState&r.modemStateMask)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (r *Redirector) logf(format string, v ...interface{}) {
	if r.log != nil {
		r.log.Printf(format, v...)
	}
}

func (r *Redirector) sendError(err error) {
	select {
	case r.errChan <- err:
	default:
		// Channel full, usually means we already have an error or exit signal
	}
}

func (r *Redirector) signalActivity() {
	select {
	case r.activityChan <- struct{}{}:
	default:
	}
}

func (r *Redirector) restoreSettings() {
	if r.deviceFd >= 0 && r.initialPortSettings != nil {
		setTermios(r.deviceFd, r.initialPortSettings)
	}
}

func (r *Redirector) setupSerialPort(fd int) error {
	settings, err := getTermios(fd)
	if err != nil {
		return fmt.Errorf("tcgetattr: %w", err)
	}

	// cfmakeraw logic
	settings.Iflag &^= (unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON)
	settings.Oflag &^= unix.OPOST
	settings.Lflag &^= (unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN)
	settings.Cflag &^= (unix.CSIZE | unix.PARENB)
	settings.Cflag |= unix.CS8

	// Specifics from C code
	settings.Cflag |= (unix.HUPCL | unix.CLOCAL)
	settings.Iflag &^= unix.IGNBRK
	settings.Iflag |= unix.BRKINT

	if err := setTermios(fd, settings); err != nil {
		return fmt.Errorf("tcsetattr: %w", err)
	}

	// Reset to blocking mode
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err == nil {
		flags &^= unix.O_NONBLOCK
		unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
	} else {
		r.logf("Unable to reset device to non blocking mode, ignoring: %v", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Telnet State Machine
// -----------------------------------------------------------------------------

type telnetStateMachine struct {
	state      int
	commandBuf []byte
	tnOpts     [256]struct {
		sentWill, sentDo, sentWont, sentDont bool
		isWill, isDo                         bool
	}
}

const (
	stateNormal = iota
	stateIACReceived
	stateIACCommand
)

func newTelnetStateMachine() *telnetStateMachine {
	return &telnetStateMachine{
		commandBuf: make([]byte, 0, 255),
	}
}

func (sm *telnetStateMachine) feed(r *Redirector, c byte) {
	switch sm.state {
	case stateNormal:
		if c == TNIAC {
			sm.state = stateIACReceived
		} else {
			// Direct write to device writer channel
			select {
			case r.devWriteChan <- []byte{c}:
			default:
				// If channel is full, we might drop or block.
				// For robustness in this model, we'll try to push.
				// In a real app, maybe handle backpressure.
			}
		}

	case stateIACReceived:
		if c == TNIAC {
			r.devWriteChan <- []byte{c}
			sm.state = stateNormal
		} else {
			sm.commandBuf = sm.commandBuf[:0]
			sm.commandBuf = append(sm.commandBuf, TNIAC, c)
			sm.state = stateIACCommand
		}

	case stateIACCommand:
		sm.commandBuf = append(sm.commandBuf, c)
		cmd := sm.commandBuf[1]

		if cmd == TNSB {
			l := len(sm.commandBuf)
			if l >= 2 && sm.commandBuf[l-2] == TNIAC && sm.commandBuf[l-1] == TNSE {
				sm.handleCommand(r)
				sm.state = stateNormal
			}
		} else if len(sm.commandBuf) == 3 {
			sm.handleCommand(r)
			sm.state = stateNormal
		} else if len(sm.commandBuf) == 2 && cmd < 240 {
			sm.state = stateNormal
		}
	}
}

func (sm *telnetStateMachine) handleCommand(r *Redirector) {
	cmd := sm.commandBuf[1]

	if cmd == TNSB {
		opt := sm.commandBuf[2]
		if opt == TNCOM_PORT_OPTION {
			r.handleCPCCommand(sm.commandBuf)
		}
		return
	}

	opt := sm.commandBuf[2]
	switch cmd {
	case TNWILL:
		if opt == TNCOM_PORT_OPTION {
			r.logf("Telnet COM Port Control Enabled (WILL).")
			r.tcpcEnabled = true
			if !sm.tnOpts[opt].sentDo {
				r.sendTelnetOption(TNDO, opt)
			}
			sm.tnOpts[opt].isDo = true
		} else if opt == TN_TRANSMIT_BINARY || opt == TN_SUPPRESS_GO_AHEAD {
			if !sm.tnOpts[opt].sentDo {
				r.sendTelnetOption(TNDO, opt)
			}
			sm.tnOpts[opt].isDo = true
		} else {
			r.sendTelnetOption(TNDONT, opt)
			sm.tnOpts[opt].isDo = false
		}
		sm.tnOpts[opt].sentDo = false

	case TNDO:
		if opt == TNCOM_PORT_OPTION {
			r.logf("Telnet COM Port Control Enabled (DO).")
			r.tcpcEnabled = true
			if !sm.tnOpts[opt].sentWill {
				r.sendTelnetOption(TNWILL, opt)
			}
			sm.tnOpts[opt].isWill = true
		} else if opt == TN_TRANSMIT_BINARY || opt == TN_SUPPRESS_GO_AHEAD {
			if !sm.tnOpts[opt].sentWill {
				r.sendTelnetOption(TNWILL, opt)
			}
			sm.tnOpts[opt].isWill = true
		} else if opt == TN_ECHO {
			r.sendTelnetOption(TNWILL, opt)
			sm.tnOpts[opt].isWill = true
		} else {
			r.sendTelnetOption(TNWONT, opt)
			sm.tnOpts[opt].isWill = false
		}
		sm.tnOpts[opt].sentWill = false
	}
}

// -----------------------------------------------------------------------------
// RFC 2217 Handlers & Helpers
// -----------------------------------------------------------------------------

func (r *Redirector) handleCPCCommand(cmd []byte) {
	if len(cmd) < 5 {
		return
	}
	subCmd := cmd[3]

	switch subCmd {
	case TNCAS_SIGNATURE:
		sig := fmt.Sprintf("SRedird %s %s", VersionId, r.deviceName)
		r.sendSignature(sig)
		r.logf("Sent signature: %s", sig)

	case TNCAS_SET_BAUDRATE:
		val := byteArrayToUint32(cmd[4:8])
		if val == 0 {
			r.logf("Baud rate notification received.")
		} else {
			r.logf("Port baud rate change to %d requested.", val)
			setPortSpeed(r.deviceFd, val)
		}
		cur := getPortSpeed(r.deviceFd)
		r.sendBaudRate(cur)

	case TNCAS_SET_DATASIZE:
		val := cmd[4]
		if val > 0 {
			r.logf("Port data size change to %d requested.", val)
			setPortDataSize(r.deviceFd, val)
		}
		cur := getPortDataSize(r.deviceFd)
		r.sendCPCByteCommand(TNASC_SET_DATASIZE, cur)

	case TNCAS_SET_PARITY:
		val := cmd[4]
		if val > 0 {
			setPortParity(r.deviceFd, val)
		}
		cur := getPortParity(r.deviceFd)
		r.sendCPCByteCommand(TNASC_SET_PARITY, cur)

	case TNCAS_SET_STOPSIZE:
		val := cmd[4]
		if val > 0 {
			setPortStopSize(r.deviceFd, val)
		}
		cur := getPortStopSize(r.deviceFd)
		r.sendCPCByteCommand(TNASC_SET_STOPSIZE, cur)

	case TNCAS_SET_CONTROL:
		val := cmd[4]
		switch val {
		case 0, 4, 7, 10, 13:
			// Query only
		case 5:
			// Break On
			unix_tcsendbreak(r.deviceFd, 1)
			r.mu.Lock()
			r.breakSignaled = true
			r.mu.Unlock()
			r.sendCPCByteCommand(TNASC_SET_CONTROL, val)
			return
		case 6:
			// Break Off
			r.mu.Lock()
			r.breakSignaled = false
			r.mu.Unlock()
			r.sendCPCByteCommand(TNASC_SET_CONTROL, val)
			return
		default:
			setPortFlowControl(r.deviceFd, val)
		}
		// Confirm
		if r.cfg.CiscoCompat && val >= 13 && val <= 16 {
			r.sendCPCByteCommand(TNASC_SET_CONTROL, 0)
		} else {
			r.sendCPCByteCommand(TNASC_SET_CONTROL, r.getPortFlowControl(0))
		}

	case TNCAS_SET_LINESTATE_MASK:
		r.lineStateMask = cmd[4] & 16
		r.sendCPCByteCommand(TNASC_SET_LINESTATE_MASK, r.lineStateMask)

	case TNCAS_SET_MODEMSTATE_MASK:
		r.modemStateMask = cmd[4]
		r.sendCPCByteCommand(TNASC_SET_MODEMSTATE_MASK, r.modemStateMask)

	case TNCAS_PURGE_DATA:
		val := cmd[4]
		switch val {
		case 1:
			tcflush(r.deviceFd, unix.TCIFLUSH)
		case 2:
			tcflush(r.deviceFd, unix.TCOFLUSH)
		case 3:
			tcflush(r.deviceFd, unix.TCIOFLUSH)
		}
		r.sendCPCByteCommand(TNASC_PURGE_DATA, val)

	case TNCAS_FLOWCONTROL_SUSPEND:
		r.logf("Flow control suspend requested.")
		r.inputFlow = false

	case TNCAS_FLOWCONTROL_RESUME:
		r.logf("Flow control resume requested.")
		r.inputFlow = true
	}
}

func (r *Redirector) sendInitialNegotiation() {
	r.sendTelnetOption(TNWILL, TN_TRANSMIT_BINARY)
	r.sendTelnetOption(TNDO, TN_TRANSMIT_BINARY)
	r.sendTelnetOption(TNWILL, TN_ECHO)
	r.sendTelnetOption(TNWILL, TN_SUPPRESS_GO_AHEAD)
	r.sendTelnetOption(TNDO, TN_SUPPRESS_GO_AHEAD)
	r.sendTelnetOption(TNDO, TNCOM_PORT_OPTION)
}

func (r *Redirector) sendTelnetOption(cmd, opt byte) {
	r.netWriteChan <- []byte{TNIAC, cmd, opt}
}

func (r *Redirector) sendCPCByteCommand(cmd, param byte) {
	pkt := []byte{TNIAC, TNSB, TNCOM_PORT_OPTION, cmd}
	if param == TNIAC {
		pkt = append(pkt, TNIAC, TNIAC)
	} else {
		pkt = append(pkt, param)
	}
	pkt = append(pkt, TNIAC, TNSE)
	r.netWriteChan <- pkt
}

func (r *Redirector) sendSignature(sig string) {
	pkt := []byte{TNIAC, TNSB, TNCOM_PORT_OPTION, TNASC_SIGNATURE}
	for i := 0; i < len(sig); i++ {
		if sig[i] == TNIAC {
			pkt = append(pkt, TNIAC)
		}
		pkt = append(pkt, sig[i])
	}
	pkt = append(pkt, TNIAC, TNSE)
	r.netWriteChan <- pkt
}

func (r *Redirector) sendBaudRate(baud uint32) {
	b := make([]byte, 4)
	b[0] = byte(baud >> 24)
	b[1] = byte(baud >> 16)
	b[2] = byte(baud >> 8)
	b[3] = byte(baud)

	pkt := []byte{TNIAC, TNSB, TNCOM_PORT_OPTION, TNASC_SET_BAUDRATE}
	for _, v := range b {
		if v == TNIAC {
			pkt = append(pkt, TNIAC)
		}
		pkt = append(pkt, v)
	}
	pkt = append(pkt, TNIAC, TNSE)
	r.netWriteChan <- pkt
}

// -----------------------------------------------------------------------------
// Termios Wrappers & System Calls
// -----------------------------------------------------------------------------

func byteArrayToUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func getTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}

func setTermios(fd int, term *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETSW, term)
}

func tcflush(fd int, queue int) {
	unix.IoctlSetInt(fd, unix.TCFLSH, queue)
}

func unix_tcsendbreak(fd int, duration int) {
	unix.IoctlSetInt(fd, unix.TCSBRK, duration)
}

// -----------------------------------------------------------------------------
// Port Attribute Getters/Setters
// -----------------------------------------------------------------------------

func getPortSpeed(fd int) uint32 {
	t, _ := getTermios(fd)
	if t == nil {
		return 0
	}
	switch t.Cflag & unix.CBAUD {
	case unix.B50:
		return 50
	case unix.B75:
		return 75
	case unix.B110:
		return 110
	case unix.B134:
		return 134
	case unix.B150:
		return 150
	case unix.B200:
		return 200
	case unix.B300:
		return 300
	case unix.B600:
		return 600
	case unix.B1200:
		return 1200
	case unix.B1800:
		return 1800
	case unix.B2400:
		return 2400
	case unix.B4800:
		return 4800
	case unix.B9600:
		return 9600
	case unix.B19200:
		return 19200
	case unix.B38400:
		return 38400
	case unix.B57600:
		return 57600
	case unix.B115200:
		return 115200
	case unix.B230400:
		return 230400
	default:
		return 0
	}
}

func setPortSpeed(fd int, baud uint32) {
	var speed uint32
	switch baud {
	case 50:
		speed = unix.B50
	case 75:
		speed = unix.B75
	case 110:
		speed = unix.B110
	case 134:
		speed = unix.B134
	case 150:
		speed = unix.B150
	case 200:
		speed = unix.B200
	case 300:
		speed = unix.B300
	case 600:
		speed = unix.B600
	case 1200:
		speed = unix.B1200
	case 1800:
		speed = unix.B1800
	case 2400:
		speed = unix.B2400
	case 4800:
		speed = unix.B4800
	case 9600:
		speed = unix.B9600
	case 19200:
		speed = unix.B19200
	case 38400:
		speed = unix.B38400
	case 57600:
		speed = unix.B57600
	case 115200:
		speed = unix.B115200
	case 230400:
		speed = unix.B230400
	default:
		speed = unix.B9600
	}
	t, err := getTermios(fd)
	if err == nil {
		t.Cflag &^= unix.CBAUD
		t.Cflag |= speed
		setTermios(fd, t)
	}
}

func getPortDataSize(fd int) byte {
	t, err := getTermios(fd)
	if err != nil {
		return 0
	}
	switch t.Cflag & unix.CSIZE {
	case unix.CS5:
		return 5
	case unix.CS6:
		return 6
	case unix.CS7:
		return 7
	case unix.CS8:
		return 8
	}
	return 0
}

func setPortDataSize(fd int, size byte) {
	t, err := getTermios(fd)
	if err != nil {
		return
	}
	t.Cflag &^= unix.CSIZE
	switch size {
	case 5:
		t.Cflag |= unix.CS5
	case 6:
		t.Cflag |= unix.CS6
	case 7:
		t.Cflag |= unix.CS7
	case 8:
		t.Cflag |= unix.CS8
	default:
		t.Cflag |= unix.CS8
	}
	setTermios(fd, t)
}

func getPortParity(fd int) byte {
	t, err := getTermios(fd)
	if err != nil {
		return 0
	}
	if (t.Cflag & unix.PARENB) == 0 {
		return 1 // None
	}
	if (t.Cflag & unix.PARODD) != 0 {
		return 2 // Odd
	}
	return 3 // Even
}

func setPortParity(fd int, parity byte) {
	t, err := getTermios(fd)
	if err != nil {
		return
	}
	switch parity {
	case 1:
		t.Cflag &^= unix.PARENB
	case 2:
		t.Cflag |= (unix.PARENB | unix.PARODD)
	case 3:
		t.Cflag |= unix.PARENB
		t.Cflag &^= unix.PARODD
	default:
		t.Cflag &^= unix.PARENB
	}
	setTermios(fd, t)
}

func getPortStopSize(fd int) byte {
	t, err := getTermios(fd)
	if err != nil {
		return 0
	}
	if (t.Cflag & unix.CSTOPB) == 0 {
		return 1
	}
	return 2
}

func setPortStopSize(fd int, size byte) {
	t, err := getTermios(fd)
	if err != nil {
		return
	}
	if size == 2 {
		t.Cflag |= unix.CSTOPB
	} else {
		t.Cflag &^= unix.CSTOPB
	}
	setTermios(fd, t)
}

func (r *Redirector) getPortFlowControl(which byte) byte {
	t, err := getTermios(r.deviceFd)
	if err != nil {
		return 1
	}
	lines, _ := unix.IoctlGetInt(r.deviceFd, unix.TIOCMGET)
	switch which {
	case 0: // Outbound/Both
		if (t.Iflag & unix.IXON) != 0 {
			return 2 // XON/XOFF
		}
		if (t.Cflag & unix.CRTSCTS) != 0 {
			return 3 // Hardware
		}
		return 1 // None
	case 4: // Break State
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.breakSignaled {
			return 5
		}
		return 6
	case 7: // DTR
		if (lines & unix.TIOCM_DTR) != 0 {
			return 8
		}
		return 9
	case 10: // RTS
		if (lines & unix.TIOCM_RTS) != 0 {
			return 11
		}
		return 12
	default:
		return 1
	}
}

func setPortFlowControl(fd int, how byte) {
	t, err := getTermios(fd)
	if err != nil {
		return
	}
	lines, _ := unix.IoctlGetInt(fd, unix.TIOCMGET)
	switch how {
	case 1: // None
		t.Iflag &^= (unix.IXON | unix.IXOFF)
		t.Cflag &^= unix.CRTSCTS
	case 2: // Xon/Xoff
		t.Iflag |= (unix.IXON | unix.IXOFF)
		t.Cflag &^= unix.CRTSCTS
	case 3: // Hardware
		t.Iflag &^= (unix.IXON | unix.IXOFF)
		t.Cflag |= unix.CRTSCTS
	case 8: // DTR On
		lines |= unix.TIOCM_DTR
	case 9: // DTR Off
		lines &^= unix.TIOCM_DTR
	case 11: // RTS On
		lines |= unix.TIOCM_RTS
	case 12: // RTS Off
		lines &^= unix.TIOCM_RTS
	}
	setTermios(fd, t)
	unix.IoctlSetInt(fd, unix.TIOCMSET, lines)
}

func getModemState(fd int, pmState uint8) uint8 {
	lines, _ := unix.IoctlGetInt(fd, unix.TIOCMGET)
	var mState uint8
	if (lines & unix.TIOCM_CAR) != 0 {
		mState += 128
	}
	if (lines & unix.TIOCM_RNG) != 0 {
		mState += 64
	}
	if (lines & unix.TIOCM_DSR) != 0 {
		mState += 32
	}
	if (lines & unix.TIOCM_CTS) != 0 {
		mState += 16
	}
	if (mState & 128) != (pmState & 128) {
		mState += 8
	}
	if (mState & 64) != (pmState & 64) {
		mState += 4
	}
	if (mState & 32) != (pmState & 32) {
		mState += 2
	}
	if (mState & 16) != (pmState & 16) {
		mState += 1
	}
	return mState
}
