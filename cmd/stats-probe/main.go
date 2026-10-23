package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"panel-agent.local/panel-agent/internal/core/xray"
)

func main() {
	server := flag.String("server", "127.0.0.1:10085", "loopback Xray StatsService endpoint")
	name := flag.String("name", "", "exact Xray counter name")
	flag.Parse()
	if *name == "" {
		fmt.Fprintln(os.Stderr, "-name is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := xray.DialStats(ctx, *server)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()
	value, err := client.QueryCounter(ctx, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s=%d\n", *name, value)
}
