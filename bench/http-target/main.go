package main

import (
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

const responseBody = "Panel-Agent-D01-OK"

func main() {
	address := flag.String("listen", "127.0.0.1:18080", "HTTP listen address")
	udpAddress := flag.String("udp-listen", "", "optional loopback UDP echo listen address")
	relay := flag.String("relay", "", "relay SSH stdio to a loopback TCP endpoint")
	flag.Parse()
	if *relay != "" {
		address, err := net.ResolveTCPAddr("tcp", *relay)
		if err != nil || !address.IP.IsLoopback() {
			log.Fatal("relay target must be loopback")
		}
		conn, err := net.DialTCP("tcp", nil, address)
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
		go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
		_, _ = io.Copy(os.Stdout, conn)
		return
	}
	if *udpAddress != "" {
		address, err := net.ResolveUDPAddr("udp", *udpAddress)
		if err != nil || !address.IP.IsLoopback() {
			log.Fatal("UDP target must be loopback")
		}
		conn, err := net.ListenUDP("udp", address)
		if err != nil {
			log.Fatal(err)
		}
		defer conn.Close()
		go func() {
			buffer := make([]byte, 2048)
			for {
				n, peer, err := conn.ReadFromUDP(buffer)
				if err != nil {
					return
				}
				_, _ = conn.WriteToUDP(buffer[:n], peer)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := w.Write([]byte("alive\n")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "18")
		_, _ = w.Write([]byte(responseBody))
	})

	server := &http.Server{
		Addr:              *address,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	log.Printf("HTTP test target listening on %s", *address)
	log.Fatal(server.ListenAndServe())
}
