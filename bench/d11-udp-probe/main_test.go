package main

import (
	"bytes"
	"testing"
)

func TestSocks5UDPPayloadSkipsVariableAddressHeader(t *testing.T) {
	payload := []byte{0x12, 0x34, 0x80, 0x00}
	for name, header := range map[string][]byte{
		"ipv4":   {0, 0, 0, 1, 1, 1, 1, 1, 0, 53},
		"ipv6":   {0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 53},
		"domain": {0, 0, 0, 3, 3, 'd', 'n', 's', 0, 53},
	} {
		t.Run(name, func(t *testing.T) {
			packet := append(append([]byte{}, header...), payload...)
			got, err := socks5UDPPayload(packet)
			if err != nil {
				t.Fatalf("socks5UDPPayload() error = %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("socks5UDPPayload() = %x, want %x", got, payload)
			}
		})
	}
}

func TestSocks5UDPPayloadRejectsTruncatedHeader(t *testing.T) {
	if _, err := socks5UDPPayload([]byte{0, 0, 0, 1, 127, 0, 0, 1}); err == nil {
		t.Fatal("socks5UDPPayload() accepted a truncated address header")
	}
}
