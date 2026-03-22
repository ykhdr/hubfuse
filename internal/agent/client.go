package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// HubClient wraps a gRPC connection to the hub.
type HubClient struct {
	conn   *grpc.ClientConn
	client pb.HubFuseClient
	logger *slog.Logger
}

// DialInsecure creates a HubClient that skips server certificate verification.
// This is used for the Join RPC only, before the device has its client cert.
func DialInsecure(hubAddr string, logger *slog.Logger) (*HubClient, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // intentional for Join bootstrap
	}
	creds := credentials.NewTLS(tlsCfg)

	conn, err := grpc.NewClient(hubAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial hub %q (insecure): %w", hubAddr, err)
	}

	return &HubClient{
		conn:   conn,
		client: pb.NewHubFuseClient(conn),
		logger: logger,
	}, nil
}

// DialWithMTLS creates a HubClient using mutual TLS with the given certificates.
func DialWithMTLS(hubAddr, caCertPath, clientCertPath, clientKeyPath string, logger *slog.Logger) (*HubClient, error) {
	tlsCfg, err := common.LoadTLSClientConfig(caCertPath, clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}

	creds := credentials.NewTLS(tlsCfg)
	conn, err := grpc.NewClient(hubAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial hub %q (mTLS): %w", hubAddr, err)
	}

	return &HubClient{
		conn:   conn,
		client: pb.NewHubFuseClient(conn),
		logger: logger,
	}, nil
}

// Join calls the hub's Join RPC to register a new device and obtain certificates.
func (c *HubClient) Join(ctx context.Context, deviceID, nickname string) (*pb.JoinResponse, error) {
	resp, err := c.client.Join(ctx, &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: nickname,
	})
	if err != nil {
		return nil, fmt.Errorf("Join RPC: %w", err)
	}
	return resp, nil
}

// Register announces this device to the hub with its current shares and SSH port.
func (c *HubClient) Register(ctx context.Context, shares []*pb.Share, sshPort int) (*pb.RegisterResponse, error) {
	resp, err := c.client.Register(ctx, &pb.RegisterRequest{
		Shares:          shares,
		SshPort:         int32(sshPort),
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		return nil, fmt.Errorf("Register RPC: %w", err)
	}
	return resp, nil
}

// Rename requests a nickname change for this device.
func (c *HubClient) Rename(ctx context.Context, newNickname string) (*pb.RenameResponse, error) {
	resp, err := c.client.Rename(ctx, &pb.RenameRequest{
		NewNickname: newNickname,
	})
	if err != nil {
		return nil, fmt.Errorf("Rename RPC: %w", err)
	}
	return resp, nil
}

// Heartbeat sends a liveness ping to the hub.
func (c *HubClient) Heartbeat(ctx context.Context) error {
	_, err := c.client.Heartbeat(ctx, &pb.HeartbeatRequest{})
	if err != nil {
		return fmt.Errorf("Heartbeat RPC: %w", err)
	}
	return nil
}

// UpdateShares pushes the current share list to the hub.
func (c *HubClient) UpdateShares(ctx context.Context, shares []*pb.Share) error {
	_, err := c.client.UpdateShares(ctx, &pb.UpdateSharesRequest{
		Shares: shares,
	})
	if err != nil {
		return fmt.Errorf("UpdateShares RPC: %w", err)
	}
	return nil
}

// Deregister removes this device from the hub.
func (c *HubClient) Deregister(ctx context.Context) error {
	_, err := c.client.Deregister(ctx, &pb.DeregisterRequest{})
	if err != nil {
		return fmt.Errorf("Deregister RPC: %w", err)
	}
	return nil
}

// Subscribe opens a server-streaming RPC to receive events from the hub.
func (c *HubClient) Subscribe(ctx context.Context, deviceID string) (pb.HubFuse_SubscribeClient, error) {
	stream, err := c.client.Subscribe(ctx, &pb.SubscribeRequest{
		DeviceId: deviceID,
	})
	if err != nil {
		return nil, fmt.Errorf("Subscribe RPC: %w", err)
	}
	return stream, nil
}

// RequestPairing initiates pairing with another device and returns the invite code.
func (c *HubClient) RequestPairing(ctx context.Context, toDevice, publicKey string) (string, error) {
	resp, err := c.client.RequestPairing(ctx, &pb.RequestPairingRequest{
		ToDevice:  toDevice,
		PublicKey: publicKey,
	})
	if err != nil {
		return "", fmt.Errorf("RequestPairing RPC: %w", err)
	}
	return resp.InviteCode, nil
}

// ConfirmPairing completes a pairing handshake and returns the peer's public key.
func (c *HubClient) ConfirmPairing(ctx context.Context, deviceID, inviteCode, publicKey string) (string, error) {
	resp, err := c.client.ConfirmPairing(ctx, &pb.ConfirmPairingRequest{
		DeviceId:  deviceID,
		InviteCode: inviteCode,
		PublicKey: publicKey,
	})
	if err != nil {
		return "", fmt.Errorf("ConfirmPairing RPC: %w", err)
	}
	return resp.PeerPublicKey, nil
}

// Close shuts down the underlying gRPC connection.
func (c *HubClient) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("close hub connection: %w", err)
	}
	return nil
}
