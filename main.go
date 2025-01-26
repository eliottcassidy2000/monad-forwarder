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
)

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
