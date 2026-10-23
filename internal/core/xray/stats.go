package xray

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	command "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrStatNotFound means Xray returned no counter with the requested exact name.
var ErrStatNotFound = errors.New("xray stat not found")

// StatsClient reads cumulative counters from a local Xray StatsService.
type StatsClient struct {
	conn    *grpc.ClientConn
	service command.StatsServiceClient
}

// DialStats connects to a loopback-only Xray API endpoint. Xray's local API uses
// plaintext gRPC, so accepting a remote address would expose it without TLS.
func DialStats(ctx context.Context, address string) (*StatsClient, error) {
	if ctx == nil {
		return nil, errors.New("xray stats: context is nil")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("xray stats: invalid endpoint %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("xray stats: endpoint host %q must be a loopback IP", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("xray stats: invalid endpoint port %q", portText)
	}

	conn, err := grpc.DialContext(
		ctx,
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("xray stats: connect to %s: %w", address, err)
	}
	return &StatsClient{conn: conn, service: command.NewStatsServiceClient(conn)}, nil
}

// QueryCounter returns a cumulative counter without resetting it. Xray treats
// Pattern as a filter, so the result is matched again by exact name here.
func (c *StatsClient) QueryCounter(ctx context.Context, name string) (int64, error) {
	if ctx == nil {
		return 0, errors.New("xray stats: context is nil")
	}
	if name == "" {
		return 0, errors.New("xray stats: counter name is empty")
	}
	if c == nil || c.service == nil {
		return 0, errors.New("xray stats: client is not connected")
	}
	response, err := c.service.QueryStats(ctx, &command.QueryStatsRequest{
		Pattern: name,
		Reset_:  false,
	})
	if err != nil {
		return 0, fmt.Errorf("xray stats: query %q: %w", name, err)
	}
	if response == nil {
		return 0, fmt.Errorf("xray stats: query %q returned an empty response", name)
	}
	for _, stat := range response.GetStat() {
		if stat != nil && stat.GetName() == name {
			return stat.GetValue(), nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrStatNotFound, name)
}

// Close closes the underlying gRPC connection.
func (c *StatsClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
