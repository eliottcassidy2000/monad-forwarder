package main

import (
	"context"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/cni/pkg/types"
	cniTypesV1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/eliottcassidy2000/monad-forwarder/internal/daemon_client"
)

func cmdAdd(args *skel.CmdArgs) error {

	// Connect to daemon
	client, err := daemon_client.NewClient("/var/run/tsnet-daemon.sock")
	if err != nil {
		return err
	}
	defer client.Close()

	// Allocate IP
	ip, err := client.Allocate(context.Background(), args.ContainerID)
	if err != nil {
		return err
	}
	// ipcfg := &types.IPConfig{
	// 	Version: "4",
	// 	Address: *ip,
	// 	Gateway: nil,
	// }
	res := &cniTypesV1.IPConfig{Address: *ip}
	res2 := &cniTypesV1.Result{IPs: []*cniTypesV1.IPConfig{res}}
	return types.PrintResult(res2, "1.0.0")
	// return types.PrintResult(&types.Result{
	// 	Version: types.Current(),
	// 	IPs: []*types.IPConfig{{
	// 		Version: "4",
	// 		Address: *ip,
	// 		Gateway: nil,
	// 	}},
	// }, types.Current())			
	// Build CNI result
	// result := &types.Result{
	// 	Version: types.Current(),
	// 	IPs: []*types.IPConfig{{
	// 		Version: "4",
	// 		Address: *ip,
	// 		Gateway: nil,
	// 	}},
	// }
	// return result.Print()
	// result := &current.Result{
	// 	CNIVersion: types.Current(),
	// 	IPs: []*current.IPConfig{{
	// 		Version: "4",
	// 		Address: *ip,
	// 		Gateway: nil,
	// 	}},
	// }
	// return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	client, err := daemon_client.NewClient("/var/run/tsnet-daemon.sock")
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Release(context.Background(), args.ContainerID)
}

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{Add:  cmdAdd,
Del: cmdDel, Check: cmdCheck}, version.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1", "0.4.0", "1.0.0", "1.1.0"),"tsnet-cni")
}

func cmdCheck(args *skel.CmdArgs) error { return nil }
