package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"os"
	"bytes"
	"net"
	"path/filepath"
	"github.com/hashicorp/nomad/api"
	"tailscale.com/tsnet"
)



func getTailscaleIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		// Match the interface name commonly used by Tailscale
		if !strings.Contains(strings.ToLower(iface.Name), "tailscale") {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Only return an IPv4 address in the 100.x.x.x CGNAT range
			if ip != nil && ip.To4() != nil && strings.HasPrefix(ip.String(), "100.") {
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("Tailscale IP not found")
}

type njc struct {
	server *tsnet.Server
	pml []api.PortMapping
}

type AuthKeyRequest struct {
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
}

type AuthKeyResponse struct {
	Key string `json:"key"`
}

// generateAuthKey calls Tailscale API to create an auth key for our epherreal tsnet services
func generateAuthKey(apiKey string) (string, error) {
	var reqBody AuthKeyRequest
	reqBody.Capabilities.Devices.Create.Reusable = true
	reqBody.Capabilities.Devices.Create.Ephemeral = true
	reqBody.Capabilities.Devices.Create.Preauthorized = true
	reqBody.Capabilities.Devices.Create.Tags = []string{"tag:nomad-service"}
	body, _ := json.Marshal(reqBody)

	url := "https://api.tailscale.com/api/v2/tailnet/-/keys?all=true"
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to create auth key: %s", resp.Status)
	}
	//fmt.Println("resp:", resp)
	defer resp.Body.Close()

	var result AuthKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	//fmt.Println("result:", result)
	return result.Key, nil
}

func keepUpdated(client *api.Client, njcd map[string]*njc) {
	// Get the agent self
	agent, err := client.Agent().Self()
	if err != nil {
		fmt.Println("Error getting agent self:", err)
		return
	}

	// Get the ID for the node to look up allocations with
	c := agent.Stats["client"]
	id := c["node_id"]

	// Get allocations for this particular node
	allocs, _, err := client.Nodes().Allocations(id, nil)
	if err != nil {
		fmt.Println("Error getting allocations:", err)
		return
	}
	fmt.Println("allocs:", allocs)
	njcUpdate(allocs, njcd)
}


func njcUpdate(allocs []*api.Allocation, njcd map[string]*njc) {
	seenAllocs:=[]string{}
	for _, alloc := range allocs {
		toAdd := []api.PortMapping{}
		toDelete := []api.PortMapping{}
		seenAllocs = append(seenAllocs, alloc.ID)
		nj, ok := njcd[alloc.ID]
		if !ok {
			apiKey := os.Getenv("TS_API_KEY")
			authKey, err := generateAuthKey(apiKey)
			if err != nil {
				fmt.Println("Error generating auth key:", err)
				os.Exit(1)
			}
			dir := filepath.Join(os.TempDir(), "tsnet-"+alloc.ID)
			nj = &njc{
				server:&tsnet.Server{										
					Hostname:  alloc.Name,
					Ephemeral: true,
					AuthKey:   authKey,
					Dir: 	   dir,
				},
				pml:           alloc.AllocatedResources.Shared.Ports,
			}
			toAdd = nj.pml
			njcd[alloc.ID] = nj
		} else{
			toDelete, toAdd = diff2(nj.pml, alloc.AllocatedResources.Shared.Ports)
		}
		for _, pm := range toAdd {
			go func(pm api.PortMapping) {
				ln, err := nj.server.Listen("tcp", fmt.Sprintf(":%d", pm.To))
				if err != nil {
					fmt.Println("TCP listen error:", err)
					return
				}
				for {
					conn, err := ln.Accept()
					if err != nil {
						fmt.Println("TCP accept error:", err)
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						target, err := net.Dial("tcp", fmt.Sprintf("%s:%d", pm.HostIP, pm.Value))
						if err != nil {
							fmt.Println("TCP dial error:", err)
							return
						}
						defer target.Close()
						go io.Copy(c, target)
						io.Copy(target, c)
					}(conn)
				}
			}(pm)
		}
		for _, pm := range toDelete {
			fmt.Println("todo", pm)
		}
	}
	toDelete := diff(getMapKeys(njcd), seenAllocs)
	for _, id := range toDelete {
		_, ok := njcd[id]
		if !ok {
			os.Exit(1)
		}
		delete(njcd, id)
	}
}




func diff2(a, b []api.PortMapping) ([]api.PortMapping, []api.PortMapping) {
    bSet := make(map[api.PortMapping]struct{})
    for _, item := range b {
        bSet[item] = struct{}{}
    }

    aSet := make(map[api.PortMapping]struct{})
    for _, item := range a {
        aSet[item] = struct{}{}
    }

    var aOnly []api.PortMapping
    for _, item := range a {
        if _, found := bSet[item]; !found {
            aOnly = append(aOnly, item)
        }
    }

    var bOnly []api.PortMapping
    for _, item := range b {
        if _, found := aSet[item]; !found {
            bOnly = append(bOnly, item)
        }
    }

    return aOnly, bOnly
}

func diff(a, b []string) []string {
    bSet := make(map[string]struct{})
    for _, item := range b {
        bSet[item] = struct{}{}
    }
    var result []string
    for _, item := range a {
        if _, found := bSet[item]; !found {
            result = append(result, item)
        }
    }
    return result
}
func getMapKeys[K comparable, V any](m map[K]V) []K {
    keys := make([]K, 0, len(m))
    for k := range m {
        keys = append(keys, k)
    }
    return keys
}

func testAllTCPPorts(njcd map[string]*njc) {
	for allocID, nj := range njcd {
		ip4, _  := nj.server.TailscaleIPs()
		if  !ip4.IsValid() {
			fmt.Printf("Error getting Tailscale IPs for %s: %v\n", allocID, ip4)
			return
		}
		for _, pm := range nj.pml {
			addr := fmt.Sprintf("%s:%d", ip4, pm.To)
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				fmt.Printf("TCP FAILED: %s (%s) -> %s\n", allocID, pm.Label, addr)
			} else {
				fmt.Printf("TCP OK: %s (%s) -> %s\n", allocID, pm.Label, addr)
				conn.Close()
			}
		}
	}
}






// OLD 

const (
	masterServerURL = "http://100.78.218.70:4646"
	cacheTTL        = 30 * time.Second // Cache expiration time
)

type Service struct {
	Address string `json:"Address"`
	Port int `json:"Port"`
}

type CacheEntry struct {
	Address   string
	Port      int
	ExpiresAt time.Time
}

var (
	client     = &http.Client{} // Shared HTTP client
	serviceMap = sync.Map{}     // Cache for service lookups
	cacheMutex = sync.Mutex{}   // Mutex for cleanup tasks
)

func main() {
	calculatedHostIP, err := getTailscaleIP()
	if err != nil {
		fmt.Println("Error getting Tailscale IP:", err)
		return
	}
	fmt.Println("tailscale ip:", calculatedHostIP)
	// Create a new Nomad client
	cfg := api.DefaultConfig()
	cfg.Address = "http://death-star:4646"
	//cfg.Address = "http://100.96.31.66:4646"
	//cfg.Address = "http://"calculatedHostIP+":4646"
	cfg.SecretID = os.Getenv("NOMAD_TOKEN")
	client, err := api.NewClient(cfg)
	if err != nil {
		fmt.Println("Error creating Nomad client:", err)
		return
	}
	// dict of services on this node, that also contains their port mappings
	njcd := make(map[string]*njc)
	for {
		keepUpdated(client, njcd)
		fmt.Println("njcd:", njcd)
		testAllTCPPorts(njcd)
		time.Sleep(10 * time.Second)
	}
}
