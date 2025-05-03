package internal

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"tailscale.com/tsnet"

	pb "github.com/eliottcassidy2000/monad-forwarder/internal/daemon_proto"
)

// server holds active tsnet instances mapped by nodeID.
type server struct {
	pb.UnimplementedDaemonServer
	mu      sync.Mutex
	nodes   map[string]*tsnet.Server
	baseDir string
}

// NewServer initializes a new daemon server.
func NewServer(baseDir string) *server {
	return &server{
		nodes:   make(map[string]*tsnet.Server),
		baseDir: baseDir,
	}
}

// Allocate spins up a tsnet.Server for the given nodeID or returns existing.
func (s *server) Allocate(ctx context.Context, req *pb.AllocateRequest) (*pb.AllocateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.nodes[req.NodeId]; !ok {
		dir := fmt.Sprintf("%s/%s", s.baseDir, req.NodeId)
		os.MkdirAll(dir, 0700)
		node := &tsnet.Server{
			Dir:     dir,
			AuthKey: os.Getenv("TS_AUTH_KEY"),
		}
		go func() {
			if err := node.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "tsnet start error: %v\n", err)
			}
		}()
		// wait for IP to be assigned
		deadline := time.After(30 * time.Second)
		ticker := time.Tick(500 * time.Millisecond)
		for {
			select {
			case <-deadline:
				return nil, fmt.Errorf("timeout waiting for tsnet IP")
			case <-ticker:
				ip, _ := node.TailscaleIPs()
				return &pb.AllocateResponse{Ip: ip.String(), Prefix: 32}, nil
			}
		}
	}
	return nil, fmt.Errorf("no IPv4 address for existing node %s", req.NodeId)
}

// Release stops and cleans up the tsnet.Server for the given nodeID.
func (s *server) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node, ok := s.nodes[req.NodeId]; ok {
		node.Close()
		delete(s.nodes, req.NodeId)
		dir := fmt.Sprintf("%s/%s", s.baseDir, req.NodeId)
		os.RemoveAll(dir)
	}
	return &pb.ReleaseResponse{}, nil
}
