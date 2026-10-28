package hysteria

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestTrafficAndKick(t *testing.T) {
	var kicked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != testSecret {
			t.Error("missing API secret")
		}
		switch r.URL.Path {
		case "/traffic":
			if r.Method != "GET" || r.URL.RawQuery != "" {
				t.Error("counter reads must not reset")
			}
			fmt.Fprint(w, `{"user-node":{"tx":9007199254740993,"rx":23}}`)
		case "/kick":
			if r.Method != "POST" {
				t.Error("invalid kick method")
			}
			kicked = true
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	api, err := NewAPI(strings.TrimPrefix(server.URL, "http://"), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	counters, err := api.Traffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counters["user-node"] != (Counter{TX: 9007199254740993, RX: 23}) {
		t.Fatal("counter lost precision or direction")
	}
	if err := api.Kick(context.Background(), []string{"user-node"}); err != nil {
		t.Fatal(err)
	}
	if !kicked {
		t.Fatal("kick not sent")
	}
}

func TestRejectInvalidTraffic(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{"id":{"tx":-1,"rx":0}}`, `{"id":{"tx":9223372036854775808,"rx":0}}`,
		`{"id":{"tx":0}}`, `{"id":{"tx":null,"rx":0}}`,
		`{"id":{"tx":0,"rx":0},"id":{"tx":1,"rx":1}}`, `{} {}`,
		strings.Repeat(" ", maxResponseBytes+1),
	} {
		t.Run(fmt.Sprint(len(body), body[:min(len(body), 32)]), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			api, err := NewAPI(strings.TrimPrefix(server.URL, "http://"), testSecret)
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			if _, err := api.Traffic(context.Background()); err == nil {
				t.Fatal("invalid traffic accepted")
			}
		})
	}
}

func TestAPIRejectsRemoteEndpointsAndRedirects(t *testing.T) {
	for _, address := range []string{"localhost:9999", "192.0.2.1:9999", "127.0.0.1:0", "127.0.0.1:99999"} {
		if _, err := NewAPI(address, testSecret); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	api, err := NewAPI(strings.TrimPrefix(server.URL, "http://"), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.Traffic(context.Background()); err == nil {
		t.Fatal("accepted redirect")
	}
}
