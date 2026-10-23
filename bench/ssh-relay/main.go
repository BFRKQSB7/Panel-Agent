// ssh-relay is a local-only benchmark helper for hosts that allow exec but
// disable SSH forwarding. It uses existing nc and changes no server settings.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	destination := flag.String("destination", "", "SSH user@host")
	port := flag.Int("port", 22, "SSH port")
	identity := flag.String("identity", "", "private key path")
	hosts := flag.String("known-hosts", "", "existing known_hosts path")
	flag.Parse()
	user, host, ok := strings.Cut(*destination, "@")
	if !ok || user == "" || host == "" || *port < 1 || *port > 65535 || flag.NArg() != 0 {
		return errors.New("destination and a valid port are required")
	}
	keyBytes, err := os.ReadFile(*identity)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	clear(keyBytes)
	if err != nil {
		return err
	}
	hostKeyCallback, err := knownhosts.New(*hosts)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(host, strconv.Itoa(*port))
	tcp, err := net.DialTimeout("tcp", address, 15*time.Second)
	if err != nil {
		return err
	}
	defer tcp.Close()
	_ = tcp.SetDeadline(time.Now().Add(15 * time.Second))
	connection, channels, requests, err := ssh.NewClientConn(tcp, address, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKeyCallback,
		// This test host's existing record pins its ED25519 host key.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	})
	if err != nil {
		return err
	}
	_ = tcp.SetDeadline(time.Time{})
	client := ssh.NewClient(connection, channels, requests)
	defer client.Close()
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, localPort := range []int{10085, 18443, 18444} {
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var sessions sync.WaitGroup
	for i, localPort := range []int{10085, 18443, 18444} {
		listener := listeners[i]
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				sessions.Add(1)
				go func() {
					defer sessions.Done()
					if err := relay(client, conn, localPort); err != nil {
						log.Printf("relay port %d: %v", localPort, err)
					}
				}()
			}
		}()
	}
	fmt.Println("ready")
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-ctx.Done():
	case err := <-done:
		return err
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	_ = client.Close()
	sessions.Wait()
	return nil
}

func relay(client *ssh.Client, conn net.Conn, port int) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	session.Stderr = os.Stderr
	// The port is an internal integer from the fixed listener set.
	if err := session.Start(fmt.Sprintf("exec nc -w 30 127.0.0.1 %d", port)); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(stdin, conn)
		_ = stdin.Close()
		// Benchmark clients close after their complete HTTP or gRPC response.
		_ = session.Close()
		_ = conn.Close()
	}()
	_, err = io.Copy(conn, stdout)
	return err
}
