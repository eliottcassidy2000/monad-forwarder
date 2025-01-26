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
)

const (
	masterServerURL = "http://100.78.218.70:4646"
	cacheTTL        = 30 * time.Second // Cache expiration time
)

type Allocation struct {
	NodeID    string `json:"NodeID"`
	TaskGroup string `json:"TaskGroup"`
	Resources struct {
		Networks []struct {
			DynamicPorts []struct {
				Label string `json:"Label"`
				Value int    `json:"Value"`
			} `json:"DynamicPorts"`
		} `json:"Networks"`
	} `json:"Resources"`
}

type Node struct {
	Address string `json:"Address"`
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
	http.HandleFunc("/", handler)
	fmt.Println("Server listening on port 4645...")
	if err := http.ListenAndServe(":4645", nil); err != nil {
		panic(err)
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

	// Query Nomad master server
	allocationsURL := fmt.Sprintf("%s/v1/jobs/%s/allocations", masterServerURL, serviceName)
	resp, err := client.Get(allocationsURL)
	if err != nil {
		return "", 0, fmt.Errorf("failed to query allocations: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("unexpected response code: %d", resp.StatusCode)
	}

	var allocations []Allocation
	if err := json.NewDecoder(resp.Body).Decode(&allocations); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %v", err)
	}

	if len(allocations) == 0 {
		return "", 0, fmt.Errorf("no allocations found for service: %s", serviceName)
	}

	allocation := allocations[0]
	nodeID := allocation.NodeID
	nodeURL := fmt.Sprintf("%s/v1/nodes/%s", masterServerURL, nodeID)
	nodeResp, err := client.Get(nodeURL)
	if err != nil {
		return "", 0, fmt.Errorf("failed to query node: %v", err)
	}
	defer nodeResp.Body.Close()

	if nodeResp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("unexpected response code for node query: %d", nodeResp.StatusCode)
	}

	var node Node
	if err := json.NewDecoder(nodeResp.Body).Decode(&node); err != nil {
		return "", 0, fmt.Errorf("failed to decode node response: %v", err)
	}

	if len(allocation.Resources.Networks) == 0 || len(allocation.Resources.Networks[0].DynamicPorts) == 0 {
		return "", 0, fmt.Errorf("no network information available")
	}

	port := allocation.Resources.Networks[0].DynamicPorts[0].Value
	entry := CacheEntry{
		Address:   node.Address,
		Port:      port,
		ExpiresAt: time.Now().Add(cacheTTL),
	}

	// Store in cache
	serviceMap.Store(serviceName, entry)
	return node.Address, port, nil
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
