// session-probe observes a persistent TCP stream and a SOCKS5 UDP association.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"panel-agent.local/panel-agent/internal/storage"
)

type observation struct {
	TCPPackets int    `json:"tcp_packets"`
	UDPPackets int    `json:"udp_packets"`
	TCPClosed  bool   `json:"tcp_closed"`
	UDPClosed  bool   `json:"udp_closed"`
	Error      string `json:"error,omitempty"`
}

func main() {
	socks := flag.String("socks", "127.0.0.1:10808", "loopback SOCKS5 endpoint")
	output := flag.String("output", "", "observation JSON file")
	flag.Parse()
	var mu sync.Mutex
	var observed observation
	write := func(change func(*observation)) {
		mu.Lock()
		defer mu.Unlock()
		change(&observed)
		value, _ := json.Marshal(observed)
		if err := storage.WritePrivateAtomic(*output, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := observeTCP(ctx, *socks, func() { write(func(o *observation) { o.TCPPackets++ }) })
		write(func(o *observation) {
			o.TCPClosed = true
			if err != nil {
				o.Error = "TCP stream closed"
			}
		})
	}()
	go func() {
		defer wg.Done()
		err := observeUDP(ctx, *socks, func() { write(func(o *observation) { o.UDPPackets++ }) })
		write(func(o *observation) {
			o.UDPClosed = true
			if err != nil {
				o.Error = "UDP association stopped receiving"
			}
		})
	}()
	wg.Wait()
}

func observeTCP(ctx context.Context, socks string, onPacket func()) error {
	dialer, err := proxy.SOCKS5("tcp", socks, nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", "127.0.0.1:18080")
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(145 * time.Second))
	if _, err := io.WriteString(conn, "GET /stream HTTP/1.1\r\nHost: target\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if scanner.Text() == "alive" {
			onPacket()
		}
	}
	return scanner.Err()
}

func observeUDP(ctx context.Context, socks string, onPacket func()) error {
	control, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", socks)
	if err != nil {
		return err
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(control, reply[:2]); err != nil || reply[1] != 0 {
		return errors.New("SOCKS authentication failed")
	}
	if _, err := control.Write([]byte{5, 3, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return err
	}
	if _, err := io.ReadFull(control, reply); err != nil || reply[1] != 0 || reply[3] != 1 {
		return errors.New("SOCKS UDP association failed")
	}
	_ = control.SetDeadline(time.Time{})
	address := &net.UDPAddr{IP: net.IPv4(reply[4], reply[5], reply[6], reply[7]), Port: int(binary.BigEndian.Uint16(reply[8:]))}
	if !address.IP.IsLoopback() {
		return errors.New("SOCKS relay must be loopback")
	}
	conn, err := net.DialUDP("udp", nil, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	packet := append([]byte{0, 0, 0, 1, 127, 0, 0, 1, byte(18081 >> 8), byte(18081 & 255)}, []byte("D04-UDP")...)
	buffer := make([]byte, 2048)
	for ctx.Err() == nil {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write(packet); err != nil {
			return err
		}
		n, err := conn.Read(buffer)
		if err != nil || n < 7 || string(buffer[n-7:n]) != "D04-UDP" {
			return errors.New("UDP response missing")
		}
		onPacket()
		time.Sleep(150 * time.Millisecond)
	}
	return ctx.Err()
}
