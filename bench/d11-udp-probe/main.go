// d11-udp-probe sends one DNS request through a local SOCKS5 UDP associate.
// It is intentionally small and dependency-free so the D11 runner can copy
// it to the authorized entry host without installing a toolchain there.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	socks := flag.String("socks", "127.0.0.1:10818", "local SOCKS5 UDP endpoint")
	target := flag.String("target", "1.1.1.1:53", "literal DNS target")
	timeout := flag.Duration("timeout", 15*time.Second, "operation timeout")
	flag.Parse()

	if err := run(*socks, *target, *timeout); err != nil {
		fmt.Println("udp_probe_error:", err)
		os.Exit(1)
	}
	fmt.Println("udp_ok")
}

func run(socksAddress, targetAddress string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	target, err := net.ResolveUDPAddr("udp4", targetAddress)
	if err != nil || target.IP == nil || target.IP.To4() == nil {
		return errors.New("target must be an IPv4 address and port")
	}
	deadline := time.Now().Add(timeout)
	tcp, err := net.DialTimeout("tcp4", socksAddress, timeout)
	if err != nil {
		return err
	}
	defer tcp.Close()
	if err := tcp.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := tcp.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	var greeting [2]byte
	if _, err := io.ReadFull(tcp, greeting[:]); err != nil {
		return err
	}
	if greeting[0] != 5 || greeting[1] != 0 {
		return errors.New("SOCKS5 did not select no-auth")
	}
	request := []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}
	if _, err := tcp.Write(request); err != nil {
		return err
	}
	relay, err := readAssociateReply(tcp)
	if err != nil {
		return err
	}
	if relay.IP == nil || relay.Port == 0 || relay.IP.IsUnspecified() {
		relay = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relay.Port}
	}
	udp, err := net.DialUDP("udp4", nil, relay)
	if err != nil {
		return err
	}
	defer udp.Close()
	if err := udp.SetDeadline(deadline); err != nil {
		return err
	}
	queryID := uint16(time.Now().UnixNano())
	query, err := dnsQuery(queryID, "example.com")
	if err != nil {
		return err
	}
	packet := []byte{0, 0, 0, 1}
	packet = append(packet, target.IP.To4()...)
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], uint16(target.Port))
	packet = append(packet, port[:]...)
	packet = append(packet, query...)
	if _, err := udp.Write(packet); err != nil {
		return err
	}
	response := make([]byte, 4096)
	n, err := udp.Read(response)
	if err != nil {
		return err
	}
	if n < 4 || response[0] != 0 || response[1] != 0 || response[2] != 0 {
		return errors.New("invalid SOCKS5 UDP response header")
	}
	payload := response[4:n]
	if len(payload) < 12 || binary.BigEndian.Uint16(payload[:2]) != queryID || payload[2]&0x80 == 0 {
		return errors.New("invalid DNS response")
	}
	if payload[3]&0x0f != 0 {
		return fmt.Errorf("DNS response code %d", payload[3]&0x0f)
	}
	return nil
}

func readAssociateReply(r io.Reader) (*net.UDPAddr, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if head[0] != 5 || head[1] != 0 || head[2] != 0 {
		return nil, fmt.Errorf("SOCKS5 UDP associate failed: reply=%d", head[1])
	}
	var ip net.IP
	switch head[3] {
	case 1:
		ip = make(net.IP, net.IPv4len)
		if _, err := io.ReadFull(r, ip); err != nil {
			return nil, err
		}
	case 4:
		ip = make(net.IP, net.IPv6len)
		if _, err := io.ReadFull(r, ip); err != nil {
			return nil, err
		}
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return nil, err
		}
		host := make([]byte, int(length[0]))
		if _, err := io.ReadFull(r, host); err != nil {
			return nil, err
		}
		ip = net.ParseIP(strings.TrimSpace(string(host)))
		if ip == nil {
			return nil, errors.New("SOCKS5 returned a non-literal UDP relay")
		}
	default:
		return nil, errors.New("SOCKS5 returned an unknown address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: int(binary.BigEndian.Uint16(port[:]))}, nil
}

func dnsQuery(id uint16, name string) ([]byte, error) {
	if strings.TrimSpace(name) == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return nil, errors.New("invalid DNS name")
	}
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[0:2], id)
	binary.BigEndian.PutUint16(packet[2:4], 0x0100)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, errors.New("invalid DNS label")
		}
		packet = append(packet, byte(len(label)))
		packet = append(packet, label...)
	}
	packet = append(packet, 0, 0, 1, 0, 1)
	return packet, nil
}
