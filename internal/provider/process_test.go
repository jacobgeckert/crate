package provider

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/TheOutdoorProgrammer/crate/proto/provider"
)

type infoOnlyServer struct {
	pb.UnimplementedMusicProviderServer
}

func (infoOnlyServer) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "fake", DisplayName: "Fake", Version: "1"}, nil
}

// TestRetryRegisterRecoversLateProvider: a provider that comes up after the
// startup window is registered by the background retry.
func TestRetryRegisterRecoversLateProvider(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(nil, nil)
	go retryRegister(ctx, m, "fake", address, 50*time.Millisecond)

	time.Sleep(150 * time.Millisecond)
	if m.IsHealthy("fake") {
		t.Fatal("healthy before the provider started")
	}

	lis, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	pb.RegisterMusicProviderServer(s, infoOnlyServer{})
	go func() { _ = s.Serve(lis) }()
	defer s.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for !m.IsHealthy("fake") {
		if time.Now().After(deadline) {
			t.Fatal("provider never registered after it came up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
