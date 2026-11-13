// Command netecho is a tiny static TCP server used by e2e tests as the guest
// workload. It accepts connections, replies with one line, and closes.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
)

func main() {
	port := flag.Int("port", 8080, "TCP port to listen on")
	flag.Parse()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "netecho:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "netecho listening on", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "netecho accept:", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			fmt.Fprintln(c, "wake-ok")
		}(conn)
	}
}
