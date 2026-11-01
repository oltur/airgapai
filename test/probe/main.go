// Probe runs inside test pods and reports how a network attempt ended, so a test can tell a dropped
// packet from a closed port. It is built into a local image, as the air-gapped VM cannot pull one.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
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

// dnsQuery asks one server directly, without the pod's resolver. Any reply counts as answered.
func dnsQuery(server, name string) string {
	conn, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return classify(err)
	}
	defer conn.Close()
	msg := []byte{0x13, 0x37, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0, 0, 1, 0, 1)
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "error"
	}
	if _, err := conn.Write(msg); err != nil {
		return classify(err)
	}
	if _, err := conn.Read(make([]byte, 512)); err != nil {
		return classify(err)
	}
	return "answered"
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
	if len(os.Args) == 4 && os.Args[1] == "dns" {
		fmt.Println(dnsQuery(os.Args[2], os.Args[3]))
		return
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe connect host:port | lookup name | dns server:port name | serve :port")
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
