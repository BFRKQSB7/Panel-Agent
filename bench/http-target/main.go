package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

const responseBody = "Panel-Agent-D01-OK"

func main() {
	address := flag.String("listen", "127.0.0.1:18080", "HTTP listen address")
	flag.Parse()

	mux := http.NewServeMux()
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
