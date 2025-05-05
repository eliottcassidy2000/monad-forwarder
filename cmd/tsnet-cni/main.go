package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/cni/pkg/types"
	cniTypesV1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/eliottcassidy2000/monad-forwarder/internal/daemon_client"
)

type NetConf struct {
    CNIVersion string `json:"cniVersion"`
    Name       string `json:"name"`
    Type       string `json:"type"`
}

func parseCNIArgs() (map[string]string) {
    args := make(map[string]string)
    raw := os.Getenv("CNI_ARGS")
    if raw == "" {
        return args
    }
    for _, pair := range strings.Split(raw, ";") {
        kv := strings.SplitN(pair, "=", 2)
        if len(kv) == 2 {
            args[kv[0]] = kv[1]
        }
    }
    return args
}


func cmdAdd(args *skel.CmdArgs) error {
	conf := &NetConf{}
    if err := json.Unmarshal(args.StdinData, conf); err != nil {
        return err
    }
	fmt.Println(conf.CNIVersion)
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
	res := &cniTypesV1.IPConfig{Address: *ip}
	res2 := &cniTypesV1.Result{IPs: []*cniTypesV1.IPConfig{res}}
	return types.PrintResult(res2, conf.CNIVersion)
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
	skel.PluginMainFuncs(skel.CNIFuncs{Add:  cmdAdd, Del: cmdDel, Check: cmdCheck}, version.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1", "0.4.0", "1.0.0", "1.1.0"),"tsnet-cni")
	//skel.PluginMainFuncs(skel.CNIFuncs{Add:  cmdAdd, Del: cmdDel, Check: cmdCheck}, version.PluginSupports("1.0.0"),"tsnet-cni")
}

func cmdCheck(args *skel.CmdArgs) error { return nil }
