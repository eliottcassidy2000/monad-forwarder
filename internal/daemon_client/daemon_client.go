package daemon_client

import (
	"context"
	"fmt"
	"net"
	"time"

	pb "github.com/eliottcassidy2000/monad-forwarder/internal/daemon_proto"
	"google.golang.org/grpc"
)

type Client struct {
	conn *grpc.ClientConn
	cli  pb.DaemonClient
}

func NewClient(socketPath string) (*Client, error) {
	conn, err := grpc.Dial(
		fmt.Sprintf("unix://%s", socketPath),
		grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn, pb.NewDaemonClient(conn)}, nil
}

func (c *Client) Allocate(ctx context.Context, id string) (*net.IPNet, error) {
	resp, err := c.cli.Allocate(ctx, &pb.AllocateRequest{NodeId: id})
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(resp.Ip)
	mask := net.CIDRMask(int(resp.Prefix), 32)
	return &net.IPNet{IP: ip, Mask: mask}, nil
}

func (c *Client) Release(ctx context.Context, id string) error {
	_, err := c.cli.Release(ctx, &pb.ReleaseRequest{NodeId: id})
	return err
}

func (c *Client) Close() error { return c.conn.Close() }
