package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/eliottcassidy2000/monad-forwarder/internal/daemon_server"
	pb "github.com/eliottcassidy2000/monad-forwarder/internal/daemon_proto"
)

func main() {
	socketPath := flag.String("socket", "/var/run/tsnet-daemon.sock", "UNIX socket path")
	dataDir := flag.String("data-dir", "/var/lib/tsnet-daemon", "Directory for tsnet node state")
	flag.Parse()

	os.Remove(*socketPath)
	listener, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("failed to listen on socket: %v", err)
	}
	os.Chmod(*socketPath, 0600)

	grpcServer := grpc.NewServer()
	srv := internal.NewServer(*dataDir)
	pb.RegisterDaemonServer(grpcServer, srv)
	reflection.Register(grpcServer)

	fmt.Printf("tsnet-daemon listening on %s\n", *socketPath)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("grpc server error: %v", err)
	}
}
