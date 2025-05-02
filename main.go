// tsnet_nomad_system_job.go
// A Nomad system job agent that ensures an ephemeral tsnet node per Nomad service instance,
// capable of proxying both TCP and UDP ports over Tailscale.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"slices"

	"github.com/hashicorp/nomad/api"
	"tailscale.com/tsnet"
)

// njc bundles a tsnet server and its port mappings
// Dir holds the state directory used by tsnet.Server
// AuthKey is kept for potential server recreation
type njc struct {
    server  *tsnet.Server
    Dir     string
    AuthKey string
    pml     []api.PortMapping
	IP	    netip.Addr
}

// difference between two slices, reused from earlier
type portDiff struct {
    add    []api.PortMapping
    remove []api.PortMapping
}

func diff2(a, b []api.PortMapping) (remove, add []api.PortMapping) {
    bSet := make(map[api.PortMapping]struct{}, len(b))
    for _, item := range b {
        bSet[item] = struct{}{}
    }
    aSet := make(map[api.PortMapping]struct{}, len(a))
    for _, item := range a {
        aSet[item] = struct{}{}
    }
    for _, item := range a {
        if _, found := bSet[item]; !found {
            remove = append(remove, item)
        }
    }
    for _, item := range b {
        if _, found := aSet[item]; !found {
            add = append(add, item)
        }
    }
    return remove, add
}

// generateAuthKey calls Tailscale API to create an auth key
func generateAuthKey(apiKey string) (string, error) {
	type authReq struct {
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
    type authResp struct { Key string `json:"key"` }
	var reqBody authReq
	reqBody.Capabilities.Devices.Create.Reusable = false
	reqBody.Capabilities.Devices.Create.Ephemeral = true
	reqBody.Capabilities.Devices.Create.Preauthorized = true
	reqBody.Capabilities.Devices.Create.Tags = []string{"tag:nomad-service"}
	body, _ := json.Marshal(reqBody)
    url := fmt.Sprintf("https://api.tailscale.com/api/v2/tailnet/-/keys?all=true")
    req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
    req.SetBasicAuth(apiKey, "")
    req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return "", err
    }
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to create auth key: %s", resp.Status)
	}
    defer resp.Body.Close()
    var result authResp
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return "", err
    }
    return result.Key, nil
}


// njcUpdate reconciles the njcd map with current allocations
func njcUpdate(allocs []*api.Allocation, njcd map[string]*njc) {
    apiKey := os.Getenv("TS_API_KEY")
    seen := make([]string, 0, len(allocs))

    for _, alloc := range allocs {
        seen = append(seen, alloc.ID)
        nj, exists := njcd[alloc.ID]
        var toAdd, toRemove []api.PortMapping

        if !exists {
			dir := filepath.Join(os.TempDir(), "tsnet-"+alloc.Name)
			wrong := []string{"monad-forwarder.monad-forwarder[0]", "monad-forwarder0.monad-forwarder0[0]", "monad-forwarder1.monad-forwarder1[0]","monad-forwarder2.monad-forwarder2[0]"}
			//right := []string{"hello-world.web[0]","mysql.mysql[0]", "dokcer-test.dokcer-test[0]"}
			if slices.Contains(wrong, alloc.Name) {
				fmt.Printf("SKIPPING. dir: %s, name: %s, pm: %s\n", dir, alloc.Name, alloc.AllocatedResources.Shared.Ports)
				continue
			}else{
				fmt.Printf("CrEaTiNg. dir: %s, name: %s, pm: %s\n", dir, alloc.Name, alloc.AllocatedResources.Shared.Ports)
			}
            // new allocation: generate key and set up server
            authKey, err := generateAuthKey(apiKey)
            if err != nil {
                log.Fatalf("Error generating auth key: %v", err)
            }
            nj = &njc{Dir: dir, AuthKey: authKey, pml: alloc.AllocatedResources.Shared.Ports}
            // create tsnet server
			fm := os.Getenv("FAKE_MONAD")
			if fm == "true" {
				fmt.Printf("FAKE MONAD. dir: %s, name: %s, pm: %s\n", dir, alloc.Name, alloc.AllocatedResources.Shared.Ports)
				continue
			}
            nj.server = &tsnet.Server{Hostname: alloc.Name, Ephemeral: true, AuthKey: authKey, Dir: dir, Logf: log.Printf}
			//nj.server = &tsnet.Server{Hostname: alloc.Name, Ephemeral: true, AuthKey: authKey, Dir: dir}
			ctx := context.Background()
			err = cleanupDuplicateHostnames(ctx, alloc.Name, apiKey)
			if err != nil {
				panic(err)
			}
			status, err := nj.server.Up(ctx)
			if err != nil {
				log.Fatalf("Error starting tsnet server: %v", err)
			}
			nj.IP = status.TailscaleIPs[0]
            njcd[alloc.ID] = nj
            toAdd = nj.pml
        } else {
            // existing: compute diff in port mappings
            toRemove, toAdd = diff2(nj.pml, alloc.AllocatedResources.Shared.Ports)
			nj.pml = alloc.AllocatedResources.Shared.Ports
        }

        // teardown removed port mappings by restarting server
        if len(toRemove) > 0 {
            log.Printf("ports removed for %s, restarting tsnet server", alloc.ID)
            nj.server.Close()
            os.RemoveAll(nj.Dir)
            // recreate server fresh
            nj.server = &tsnet.Server{Hostname: alloc.Name, Ephemeral: true, AuthKey: nj.AuthKey, Dir: nj.Dir, Logf: log.Printf}
            toAdd = alloc.AllocatedResources.Shared.Ports
            nj.pml = alloc.AllocatedResources.Shared.Ports
        }

        // start listeners for new port mappings
        for _, pm := range toAdd {
			pm := pm // capture
            go startTCPProxy(nj.server, pm, nj.IP)
            go startUDPProxy(nj.server, pm, nj.IP)
        }
        nj.pml = alloc.AllocatedResources.Shared.Ports
    }

    // remove stale allocations
    // those in njcd but not in seen
    existingIDs := make([]string, 0, len(njcd))
    for id := range njcd {
        existingIDs = append(existingIDs, id)
    }
    stale := difference(existingIDs, seen)
    for _, id := range stale {
        log.Printf("allocation %s gone, cleaning up", id)
        entry := njcd[id]
        entry.server.Close()
        os.RemoveAll(entry.Dir)
        delete(njcd, id)
    }
}

// difference returns elements in a but not in b
func difference(a, b []string) []string {
    setB := make(map[string]struct{}, len(b))
    for _, v := range b {
        setB[v] = struct{}{}
    }
    var diff []string
    for _, v := range a {
        if _, ok := setB[v]; !ok {
            diff = append(diff, v)
        }
    }
    return diff
}

// startTCPProxy forwards TCP traffic from tsnet to local host
func startTCPProxy(server *tsnet.Server, pm api.PortMapping, ip netip.Addr) {
    ln, err := server.Listen("tcp", fmt.Sprintf("%s:%d",ip, pm.To))
    if err != nil {
        log.Printf("TCP listen error for %v: %v", pm, err)
        return
    }
    defer ln.Close()
    for {
        conn, err := ln.Accept()
        if err != nil {
            log.Printf("TCP accept error: %v", err)
            return
        }
        go func(c net.Conn) {
            defer c.Close()
            target, err := net.Dial("tcp", fmt.Sprintf("%s:%d", pm.HostIP, pm.Value))
            if err != nil {
                log.Printf("TCP dial error: %v", err)
                return
            }
            defer target.Close()
            go io.Copy(c, target)
            io.Copy(target, c)
        }(conn)
    }
}

// startUDPProxy forwards UDP traffic from tsnet to local host
func startUDPProxy(server *tsnet.Server, pm api.PortMapping, ip netip.Addr) {
    pconn, err := server.ListenPacket("udp", fmt.Sprintf("%s:%d",ip, pm.To))
    if err != nil {
        log.Printf("UDP listen error for %v: %v", pm, err)
        return
    }
    defer pconn.Close()
    buf := make([]byte, 65535)
    for {
        n, addr, err := pconn.ReadFrom(buf)
        if err != nil {
            log.Printf("UDP read error: %v", err)
            return
        }
        data := make([]byte, n)
        copy(data, buf[:n])
        go func(data []byte, clientAddr net.Addr) {
            conn, err := net.Dial("udp", fmt.Sprintf("%s:%d", pm.HostIP, pm.Value))
            if err != nil {
                log.Printf("UDP dial error: %v", err)
                return
            }
            defer conn.Close()
            conn.Write(data)
            respBuf := make([]byte, 65535)
            conn.SetReadDeadline(time.Now().Add(5 * time.Second))
            n2, err := conn.Read(respBuf)
            if err == nil {
                pconn.WriteTo(respBuf[:n2], clientAddr)
            }
        }(data, addr)
    }
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
	njcUpdate(allocs, njcd)
}


func testAllTCPPorts(njcd map[string]*njc) {
	for allocID, nj := range njcd {
		ip4, err := computeIP(nj.server)
		if err != nil {
			fmt.Printf("Error getting Tailscale IP: %v\n", err)
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

func computeIP(s *tsnet.Server)(string, error) {
	if err := s.Start(); err != nil {
		log.Fatalf("failed to start tsnet server: %v", err)
		return "", err
	}
	defer s.Close()

	client, err := s.LocalClient()
	if err != nil {
		log.Fatalf("failed to get local client: %v", err)
		return "", err
	}

	ctx := context.Background()
	status, err := client.Status(ctx)
	if err != nil {
		log.Fatalf("failed to get status: %v", err)
		return "", err
	}

	self := status.Self
	if self == nil {
		log.Fatalf("status.Self is nil")
		return "", fmt.Errorf("status.Self is nil")
	}

	var tailscaleIP string
	for _, addr := range self.TailscaleIPs {
		if addr.Is4() { // choose IPv4 address (e.g., 100.x.x.x)
			tailscaleIP = addr.String()
			break
		}
	}
	if tailscaleIP == "" {
		return "", fmt.Errorf("no IPv4 address found in Tailscale IPs")
	}
	return tailscaleIP, nil
}

func main() {
	calculatedHostIP, err := getTailscaleIP()
	if err != nil {
		fmt.Println("Error getting Tailscale IP:", err)
		return
	}
	fmt.Println("tailscale ip:", calculatedHostIP)
	// Create a new Nomad client
	cfg := api.DefaultConfig()
	//cfg.Address = "http://"+calculatedHostIP+":4646"
	cfg.Address = "http://127.0.0.1:4646"
	//cfg.Address = "http://death-star:4646"
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
		time.Sleep(30 * time.Second)
	}
}



const (             // your tailnet name
	apiKey      = "tskey-abcdef1234567890abcdef"  // your Tailscale API key
)

type Device struct {
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	CreatedAt time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
}

// UpdateNomadVariable safely updates or adds keys in a Nomad variable without overwriting existing keys.
func UpdateNomadVariable(client *api.Client, varPath string, updates map[string]interface{}) error {

	// Try to read the existing variable
	variable, _, err := client.Variables().Read(varPath, nil)
	if err != nil {
		if err == api.ErrVariablePathNotFound {
			// Variable doesn't exist; create a new one
			variable = &api.Variable{
				Path:  varPath,
				Items: api.VariableItems{},
			}
		} else {
			return fmt.Errorf("failed to read variable: %w", err)
		}
	}

	// Apply updates to the existing items
	if variable.Items == nil {
		variable.Items = api.VariableItems{}
	}
	for k, v := range updates {
		if strValue, ok := v.(string); ok {
			variable.Items[k] = strValue
		} else {
			return fmt.Errorf("value for key %s is not a string", k)
		}
	}

	// Write the updated variable back
	_, _, err = client.Variables().Update(variable, nil)
	if err != nil {
		return fmt.Errorf("failed to update variable: %w", err)
	}

	return nil
}

func cleanupDuplicateHostnames(ctx context.Context, hostname string, apiKey string) error {
	url := fmt.Sprintf("https://api.tailscale.com/api/v2/tailnet/-/devices")

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(apiKey, "")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error: %s - %s", resp.Status, body)
	}

	var response struct {
		Devices []Device `json:"devices"`
	}
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return err
	}

	var matches []Device
	for _, device := range response.Devices {
		if device.Hostname == hostname {
			matches = append(matches, device)
		}
	}

	if len(matches) <= 0 {
		fmt.Println("No existing found for hostname:", hostname)
		return nil
	}

	// // Sort devices by LastSeen DESC, newest first
	// sort.Slice(matches, func(i, j int) bool {
	// 	return matches[i].LastSeen.After(matches[j].LastSeen)
	// })

	// ddelte all matches
	for _, oldDevice := range matches[0:] {
		fmt.Println("Deleting stale device ID:", oldDevice.ID)
		delUrl := fmt.Sprintf("https://api.tailscale.com/api/v2/device/%s", oldDevice.ID)
		delReq, _ := http.NewRequestWithContext(ctx, "DELETE", delUrl, nil)
		delReq.SetBasicAuth(apiKey, "")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		delResp, err := http.DefaultClient.Do(delReq)
		if err != nil {
			return fmt.Errorf("error deleting device ID %s: %w", oldDevice.ID, err)
		}
		delResp.Body.Close()
		if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(delResp.Body)
			return fmt.Errorf("failed to delete device %s: %s", oldDevice.ID, body)
		}
	}

	fmt.Printf("Cleanup complete. Kept most recent device for hostname: %s\n", hostname)
	return nil
}
