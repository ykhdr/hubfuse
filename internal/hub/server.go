package hub

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc/peer"
)

// Server implements the gRPC HubFuse service.
type Server struct {
	pb.UnimplementedHubFuseServer
	registry *Registry
	logger   *slog.Logger
}

// NewServer creates a new Server backed by the given Registry.
func NewServer(registry *Registry, logger *slog.Logger) *Server {
	return &Server{
		registry: registry,
		logger:   logger,
	}
}

// Join handles first-time device registration. It does not require
// authentication — the client will receive a signed cert it can use for
// subsequent calls.
func (s *Server) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	certPEM, keyPEM, caCertPEM, err := s.registry.Join(ctx, req.DeviceId, req.Nickname)
	if err != nil {
		return &pb.JoinResponse{Success: false, Error: err.Error()}, nil
	}

	return &pb.JoinResponse{
		Success:    true,
		ClientCert: certPEM,
		ClientKey:  keyPEM,
		CaCert:     caCertPEM,
	}, nil
}

// Register marks a device as online and returns the list of currently online
// devices. The device_id is extracted from the mTLS client certificate.
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	ip := peerIP(ctx)

	online, err := s.registry.Register(ctx, deviceID, ip, int(req.SshPort), req.Shares, int(req.ProtocolVersion))
	if err != nil {
		return &pb.RegisterResponse{Success: false, Error: err.Error()}, nil
	}

	ids := make([]string, 0, len(online))
	for _, d := range online {
		ids = append(ids, d.DeviceID)
	}
	sharesByDevice, err := s.registry.store.GetSharesForDevices(ctx, ids)
	if err != nil {
		s.logger.Warn("register: get shares", slog.Any("error", err))
		sharesByDevice = map[string][]*store.Share{}
	}

	devices := make([]*pb.DeviceInfo, 0, len(online))
	for _, d := range online {
		devices = append(devices, &pb.DeviceInfo{
			DeviceId: d.DeviceID,
			Nickname: d.Nickname,
			Ip:       d.LastIP,
			SshPort:  int32(d.SSHPort),
			Shares:   sharesToProto(sharesByDevice[d.DeviceID]),
		})
	}

	return &pb.RegisterResponse{
		Success:       true,
		DevicesOnline: devices,
	}, nil
}

// Rename changes a device's nickname.
func (s *Server) Rename(ctx context.Context, req *pb.RenameRequest) (*pb.RenameResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.registry.Rename(ctx, deviceID, req.NewNickname); err != nil {
		return &pb.RenameResponse{Success: false, Error: err.Error()}, nil
	}

	return &pb.RenameResponse{Success: true}, nil
}

// Heartbeat records that a device is still alive.
func (s *Server) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.registry.Heartbeat(ctx, deviceID); err != nil {
		return &pb.HeartbeatResponse{Success: false}, nil
	}

	return &pb.HeartbeatResponse{Success: true}, nil
}

// UpdateShares replaces a device's exported shares.
func (s *Server) UpdateShares(ctx context.Context, req *pb.UpdateSharesRequest) (*pb.UpdateSharesResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.registry.UpdateShares(ctx, deviceID, req.Shares); err != nil {
		return &pb.UpdateSharesResponse{Success: false}, nil
	}

	return &pb.UpdateSharesResponse{Success: true}, nil
}

// Deregister marks a device as offline.
func (s *Server) Deregister(ctx context.Context, req *pb.DeregisterRequest) (*pb.DeregisterResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.registry.Deregister(ctx, deviceID); err != nil {
		return &pb.DeregisterResponse{Success: false}, nil
	}

	return &pb.DeregisterResponse{Success: true}, nil
}

// Subscribe opens a server-streaming RPC that pushes events to the device
// until the context is cancelled. The device_id is taken from the mTLS client
// certificate; req.DeviceId is ignored (deprecated, see proto comment).
func (s *Server) Subscribe(req *pb.SubscribeRequest, stream pb.HubFuse_SubscribeServer) error {
	deviceID, err := common.ExtractDeviceID(stream.Context())
	if err != nil {
		return err
	}

	ch, unsub := s.registry.Subscribe(deviceID)
	defer unsub()

	// Signal that the subscription is active so clients can synchronise.
	if err := stream.Send(&pb.Event{
		Payload: &pb.Event_SubscribeReady{SubscribeReady: &pb.SubscribeReadyEvent{}},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				// Channel was closed (e.g. by Deregister).
				return nil
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		}
	}
}

// RequestPairing initiates a pairing request from the authenticated device to
// another device.
func (s *Server) RequestPairing(ctx context.Context, req *pb.RequestPairingRequest) (*pb.RequestPairingResponse, error) {
	fromDevice, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	code, err := s.registry.RequestPairing(ctx, fromDevice, req.ToDevice, req.PublicKey)
	if err != nil {
		return nil, err
	}

	return &pb.RequestPairingResponse{InviteCode: code}, nil
}

// ConfirmPairing completes a pairing by validating an invite code. The
// device_id is taken from the mTLS client certificate; req.DeviceId is ignored
// (deprecated, see proto comment).
func (s *Server) ConfirmPairing(ctx context.Context, req *pb.ConfirmPairingRequest) (*pb.ConfirmPairingResponse, error) {
	deviceID, err := common.ExtractDeviceID(ctx)
	if err != nil {
		return nil, err
	}

	peerPublicKey, err := s.registry.ConfirmPairing(ctx, deviceID, req.InviteCode, req.PublicKey)
	if err != nil {
		return &pb.ConfirmPairingResponse{Success: false, Error: err.Error()}, nil
	}

	return &pb.ConfirmPairingResponse{
		Success:       true,
		PeerPublicKey: peerPublicKey,
	}, nil
}

// ListDevices returns all devices known to the hub, regardless of status.
func (s *Server) ListDevices(ctx context.Context, req *pb.ListDevicesRequest) (*pb.ListDevicesResponse, error) {
	if _, err := common.ExtractDeviceID(ctx); err != nil {
		return nil, err
	}

	all, err := s.registry.store.ListAllDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all devices: %w", err)
	}

	ids := make([]string, 0, len(all))
	for _, d := range all {
		ids = append(ids, d.DeviceID)
	}
	sharesByDevice, err := s.registry.store.GetSharesForDevices(ctx, ids)
	if err != nil {
		s.logger.Warn("ListDevices: get shares", slog.Any("error", err))
		sharesByDevice = map[string][]*store.Share{}
	}

	devices := make([]*pb.DeviceInfo, 0, len(all))
	for _, d := range all {
		devices = append(devices, &pb.DeviceInfo{
			DeviceId: d.DeviceID,
			Nickname: d.Nickname,
			Ip:       d.LastIP,
			SshPort:  int32(d.SSHPort),
			Shares:   sharesToProto(sharesByDevice[d.DeviceID]),
			Status:   d.Status,
		})
	}

	return &pb.ListDevicesResponse{Devices: devices}, nil
}

// peerIP extracts the IP address from the gRPC peer information in ctx. If
// the peer cannot be determined an empty string is returned.
func peerIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}
