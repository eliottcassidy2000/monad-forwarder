package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
			// Create a new entry if it doesn't exist
			apiKey := os.Getenv("TS_API_KEY")
			authKey, err := generateAuthKey(apiKey)
			fmt.Println("NEEEEEEEEED to create new entry for alloc:", alloc.ID, alloc.Name, authKey)
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
			fmt.Println("hello Portmapping:", pm)
			// TCP listener
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
			//go nj.server.Listen("udp", fmt.Sprintf("%s:%d", pm.HostIP, pm.To))
		}
		for _, pm := range toDelete {
			fmt.Println("bye Portmapping:", pm)
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

func handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, "/v1/") {
		forwardRequest(w, r, masterServerURL)
		return
	}

	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) < 1 {
		http.Error(w, "Invalid request path", http.StatusBadRequest)
		return
	}

	serviceName := segments[0]
	trimmedPath := "/" + strings.Join(segments[1:], "/") // Remove the service name from the path

	serviceNode, servicePort, err := lookupService(serviceName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Service lookup failed: %v", err), http.StatusInternalServerError)
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d%s", serviceNode, servicePort, trimmedPath)
	forwardRequest(w, r, targetURL)
}


func forwardRequest(w http.ResponseWriter, r *http.Request, target string) {
	proxyURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, "Invalid target URL", http.StatusInternalServerError)
		return
	}

	req, err := http.NewRequest(r.Method, proxyURL.String(), r.Body)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	copyHeader(req.Header, r.Header)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to forward request: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func lookupService(serviceName string) (string, int, error) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	// Check cache
	if cached, found := serviceMap.Load(serviceName); found {
		entry := cached.(CacheEntry)
		if time.Now().Before(entry.ExpiresAt) {
			return entry.Address, entry.Port, nil
		}
		// Remove expired entry
		serviceMap.Delete(serviceName)
	}

    // Get NOMAD_TOKEN from the environment
    nomadToken := os.Getenv("NOMAD_TOKEN")
    if nomadToken == "" {
        return "", 0, fmt.Errorf("NOMAD_TOKEN environment variable is not set")
    }

	// Query Nomad master server for service id
	serviceURL := fmt.Sprintf("%s/v1/service/%s", masterServerURL, serviceName)
    req, err := http.NewRequest("GET", serviceURL, nil)
    if err != nil {
        return "", 0, fmt.Errorf("failed to create request: %v", err)
    }
    // Add token to request headers
    req.Header.Set("X-Nomad-Token", nomadToken)

    resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to query service: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("unexpected response code: %d", resp.StatusCode)
	}

	var services []Service
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %v", err)
	}

	if len(services) == 0 {
		return "", 0, fmt.Errorf("nothing found for service: %s", serviceName)
	}

	service := services[0]
	port := service.Port
	address := service.Address

	entry := CacheEntry{
		Address:   address,
		Port:      port,
		ExpiresAt: time.Now().Add(cacheTTL),
	}

	// Store in cache
	serviceMap.Store(serviceName, entry)
	return address, port, nil
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
