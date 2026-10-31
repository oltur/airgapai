// Probe runs inside test pods and reports how a network attempt ended, so a test can tell a dropped
// packet from a closed port. It is built into a local image, as the air-gapped VM cannot pull one.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

const timeout = 5 * time.Second

func classify(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case err == nil:
		return "reached"
	case errors.As(err, &dnsErr):
		return "dns-failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EINVAL):
		return "no-route"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	}
	return "error"
}

func connect(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		conn.Close()
	}
	return classify(err)
}

func lookup(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, name); err != nil {
		return "dns-failed"
	}
	return "resolved"
}

func serve(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		conn.Close()
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe connect host:port | lookup name | serve :port")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "connect":
		fmt.Println(connect(os.Args[2]))
	case "lookup":
		fmt.Println(lookup(os.Args[2]))
	case "serve":
		if err := serve(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		os.Exit(2)
	}
}
