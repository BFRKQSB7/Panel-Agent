package xray

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	command "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

type statsService struct {
	command.UnimplementedStatsServiceServer
	request *command.QueryStatsRequest
	stats   []*command.Stat
}

func (s *statsService) QueryStats(_ context.Context, request *command.QueryStatsRequest) (*command.QueryStatsResponse, error) {
	s.request = request
	return &command.QueryStatsResponse{Stat: s.stats}, nil
}

func startStatsService(t *testing.T, service *statsService) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	command.RegisterStatsServiceServer(server, service)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func TestQueryCounterUsesExactNameAndDoesNotReset(t *testing.T) {
	name := "user>>>u0001-n01>>>traffic>>>uplink"
	service := &statsService{stats: []*command.Stat{
		{Name: name + ".suffix", Value: 999},
		{Name: name, Value: 123},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := DialStats(ctx, startStatsService(t, service))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	value, err := client.QueryCounter(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if value != 123 {
		t.Fatalf("QueryCounter() = %d, want exact counter value 123", value)
	}
	if service.request == nil || service.request.GetPattern() != name {
		t.Fatalf("query pattern = %v, want %q", service.request, name)
	}
	if service.request.GetReset_() {
		t.Fatal("QueryStats reset counters; want a non-destructive read")
	}
}

func TestQueryCounterReportsMissingExactName(t *testing.T) {
	service := &statsService{stats: []*command.Stat{{Name: "prefix-target-suffix", Value: 8}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := DialStats(ctx, startStatsService(t, service))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, err = client.QueryCounter(ctx, "target")
	if !errors.Is(err, ErrStatNotFound) {
		t.Fatalf("QueryCounter() error = %v, want ErrStatNotFound", err)
	}
}

func TestDialStatsRejectsNonLoopbackEndpoints(t *testing.T) {
	for _, address := range []string{"example.com:10085", "8.8.8.8:10085", "localhost:10085"} {
		t.Run(address, func(t *testing.T) {
			client, err := DialStats(context.Background(), address)
			if err == nil {
				_ = client.Close()
				t.Fatalf("DialStats(%q) succeeded, want loopback-only rejection", address)
			}
		})
	}
}
