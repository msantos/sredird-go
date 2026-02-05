[![Go Reference](https://pkg.go.dev/badge/go.iscode.ca/sredird.svg)](https://pkg.go.dev/go.iscode.ca/sredird)

# SYNOPSIS

sredird *option* *loglevel* *device* [*pollinginterval*]

# DESCRIPTION

sredird is:

* an [RFC 2217](https://datatracker.ietf.org/doc/html/rfc2217) compliant
  serial port redirector
* maps a network port to a serial device: serial port parameters are
  configured using an extension to the telnet protocol
* runs under a [UCSPI](http://cr.yp.to/proto/ucspi.txt) or other inetd
  style service such as systemd for process level isolation

sredird is usable as a minimal serial console service on a low powered
devices like the raspberry pi zero w.

A [picocom](https://github.com/npat-efault/picocom/tree/rfc2217) branch
supports RFC 2217.

This version of [sredird](https://github.com/msantos/sredird/) is an AI
assisted translation from C to Go.

# USAGE

loglevel
: numeric syslog level, see `syslog(3)`

device
: serial device

pollinginterval
: Poll interval is in milliseconds, default is 100, 0 means no polling

# OPTIONS

-i, --cisco-compatibility
: indicates Cisco IOS Bug compatibility

-t, --timeout *seconds*
:set inactivity timeout

# BUILDING

```bash
go install go.iscode.ca/sredird/cmd/sredird@latest
```

# ALTERNATIVES

* [sredird](https://github.com/msantos/sredird/)
