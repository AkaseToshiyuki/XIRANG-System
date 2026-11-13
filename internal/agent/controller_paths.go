package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xirang-grid/xirang/internal/networkhealth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type controllerPathCache struct {
	ServerURL string               `json:"serverUrl"`
	NodeID    string               `json:"nodeId"`
	Paths     []networkhealth.Path `json:"paths"`
}

var controllerFallbackCursor atomic.Uint64

func controllerPathKey(p networkhealth.Path) string {
	return p.SourceInterface + "/" + p.SourceAddress + "/" + p.TargetAddress
}

func loadControllerPaths(directory string, state State) []networkhealth.Path {
	f, err := os.Open(filepath.Join(directory, "network-paths.json"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var cache controllerPathCache
	if json.NewDecoder(io.LimitReader(f, 65537)).Decode(&cache) != nil || cache.ServerURL != state.ServerURL || cache.NodeID != state.NodeID || len(cache.Paths) > 16 {
		return nil
	}
	return cache.Paths
}

func pathDialer(path networkhealth.Path) (*net.Dialer, error) {
	ip := net.ParseIP(path.SourceAddress)
	if ip == nil || net.ParseIP(path.TargetAddress) == nil {
		return nil, errors.New("network path requires literal addresses")
	}
	iface, err := net.InterfaceByName(path.SourceInterface)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return nil, errors.New("network path interface is unavailable")
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	owned := false
	for _, raw := range addresses {
		if strings.Split(raw.String(), "/")[0] == path.SourceAddress {
			owned = true
		}
	}
	if !owned {
		return nil, errors.New("network path source address disappeared")
	}
	return &net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: ip}}, nil
}

func connectPathTLS(ctx context.Context, config *tls.Config, path networkhealth.Path, port string) (net.Conn, error) {
	if err := networkhealth.CheckRoute(ctx, path); err != nil {
		return nil, err
	}
	dialer, err := pathDialer(path)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	connection, err := dialer.DialContext(bounded, "tcp", net.JoinHostPort(path.TargetAddress, port))
	if err != nil {
		return nil, err
	}
	secured := tls.Client(connection, config.Clone())
	if err := secured.HandshakeContext(bounded); err != nil {
		connection.Close()
		return nil, err
	}
	return secured, nil
}

// Requests retain their original URL and TLS authority. Only the authenticated
// controller's transport address changes; client credentials never follow an
// advertised URL, arbitrary host or cross-origin redirect.
func controllerHTTPTransport(directory string, state State, config *tls.Config) *http.Transport {
	base, _ := url.Parse(state.ServerURL)
	port := base.Port()
	if port == "" {
		port = "443"
	}
	config = config.Clone()
	config.ServerName = base.Hostname()
	return &http.Transport{TLSClientConfig: config, DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(base.Hostname(), port) {
			return nil, errors.New("authenticated controller request changed origin")
		}
		for _, path := range loadControllerPaths(directory, state) {
			if connection, err := connectPathTLS(ctx, config, path, port); err == nil {
				return connection, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
		return dialer.DialContext(ctx, network, address)
	}}
}

func dialSelectedControl(ctx context.Context, state State, config ControlLoopConfig, trust *tls.Config) (*grpc.ClientConn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil {
		return nil, "", err
	}
	trust = trust.Clone()
	trust.ServerName = host
	paths := loadControllerPaths(config.StateDirectory, state)
	if len(paths) > 2 {
		start := int(controllerFallbackCursor.Add(1)-1) % (len(paths) - 1)
		paths = []networkhealth.Path{paths[0], paths[1+start]}
	}
	for _, path := range paths {
		if err := networkhealth.CheckRoute(ctx, path); err != nil {
			continue
		}
		dialer, err := pathDialer(path)
		if err != nil {
			continue
		}
		connection, err := dialControlPlane(ctx, net.JoinHostPort(path.TargetAddress, port), 3*time.Second,
			grpc.WithTransportCredentials(credentials.NewTLS(trust)),
			grpc.WithContextDialer(func(c context.Context, address string) (net.Conn, error) {
				return dialer.DialContext(c, "tcp", address)
			}),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxControlMessage), grpc.MaxCallSendMsgSize(maxControlMessage)))
		if err == nil {
			return connection, controllerPathKey(path), nil
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
	}
	connection, err := dialControlPlane(ctx, config.Address, controlDialTimeout,
		grpc.WithTransportCredentials(credentials.NewTLS(trust)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxControlMessage), grpc.MaxCallSendMsgSize(maxControlMessage)))
	return connection, "", err
}

// refreshControllerPaths runs outside the stream receive loop. It verifies the
// application endpoints before replacing a working control connection, then
// persists bounded fallback addresses for recovery after an Agent restart.
func refreshControllerPaths(ctx context.Context, state State, config ControlLoopConfig) (string, error) {
	client, err := authenticatedHTTPClient(config.StateDirectory, state)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(state.ServerURL, "/")+"/api/v1/network-paths", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("network path lookup returned %d", response.StatusCode)
	}
	var result struct {
		SourceNodeID string               `json:"sourceNodeId"`
		TargetNodeID string               `json:"targetNodeId"`
		Paths        []networkhealth.Path `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 65537)).Decode(&result); err != nil {
		return "", err
	}
	if result.SourceNodeID != state.NodeID || len(result.Paths) > 16 {
		return "", errors.New("network path response has the wrong identity or size")
	}
	if result.TargetNodeID == state.NodeID {
		return "", nil
	}
	trust, err := authenticatedTLSConfig(config.StateDirectory, state)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil {
		return "", err
	}
	trust.ServerName = host
	base, _ := url.Parse(state.ServerURL)
	apiTrust := trust.Clone()
	apiTrust.ServerName = base.Hostname()
	apiPort := base.Port()
	if apiPort == "" {
		apiPort = "443"
	}
	now := time.Now()
	var selected []networkhealth.Path
	for _, path := range result.Paths {
		if !path.ExpiresAt.After(now) || path.ExpiresAt.After(now.Add(networkhealth.ProbeMaxAge+5*time.Second)) || path.CapacityBits < 0 || path.LatencyMicros <= 0 {
			continue
		}
		if len(selected) == 0 {
			connection, err := connectPathTLS(ctx, trust, path, port)
			if err != nil {
				continue
			}
			connection.Close()
			connection, err = connectPathTLS(ctx, apiTrust, path, apiPort)
			if err != nil {
				continue
			}
			connection.Close()
		}
		selected = append(selected, path)
	}
	if len(selected) == 0 {
		return "", errors.New("no fresh reciprocal controller path passes TLS verification")
	}
	encoded, err := json.Marshal(controllerPathCache{ServerURL: state.ServerURL, NodeID: state.NodeID, Paths: selected})
	if err != nil {
		return "", err
	}
	if err = atomicWrite(filepath.Join(config.StateDirectory, "network-paths.json"), encoded, 0o600); err != nil {
		return "", err
	}
	return controllerPathKey(selected[0]), nil
}
