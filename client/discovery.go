package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// mdnsServiceType is the mDNS service type UberSDR advertises.
const mdnsServiceType = "_ubersdr._tcp"

// descriptionPath is the UberSDR API endpoint that returns receiver info
// including the list of installed add-ons.
const descriptionPath = "/api/description"

// apiDescription is the subset of /api/description we care about.
type apiDescription struct {
	Addons   []string `json:"addons"`
	Receiver struct {
		Callsign string `json:"callsign"`
		Name     string `json:"name"`
		Location string `json:"location"`
	} `json:"receiver"`
	AvailableClients int `json:"available_clients"`
	MaxClients       int `json:"max_clients"`
}

// hasDXCluster returns true if the description advertises the dxcluster addon.
func (d *apiDescription) hasDXCluster() bool {
	for _, a := range d.Addons {
		if a == dxclusterAddon {
			return true
		}
	}
	return false
}

// probeDescription fetches /api/description from host:port (plain HTTP) and
// returns the parsed response, or an error if unreachable or missing the addon.
func probeDescription(host string, port int) (*apiDescription, error) {
	url := fmt.Sprintf("http://%s:%d%s", host, port, descriptionPath)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var desc apiDescription
	if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
		return nil, err
	}
	return &desc, nil
}

// MDNSDiscovery browses the local network for UberSDR instances via mDNS
// (_ubersdr._tcp) and probes each one's /api/description to confirm the
// dxcluster add-on is installed. Results are available via Instances().
type MDNSDiscovery struct {
	mu        sync.RWMutex
	instances map[string]Instance // keyed by "host:port"
	cancel    context.CancelFunc
	onChange  func() // called (from a goroutine) when the instance list changes
}

// NewMDNSDiscovery starts an mDNS browse. Call Stop() when done.
// onChange is called whenever a new instance is confirmed; may be nil.
func NewMDNSDiscovery(onChange func()) (*MDNSDiscovery, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mDNS resolver: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &MDNSDiscovery{
		instances: make(map[string]Instance),
		cancel:    cancel,
		onChange:  onChange,
	}

	entries := make(chan *zeroconf.ServiceEntry)
	go func() {
		for entry := range entries {
			go d.handleEntry(entry) // probe in background so browse isn't blocked
		}
	}()
	go func() {
		_ = resolver.Browse(ctx, mdnsServiceType, "local.", entries)
	}()

	return d, nil
}

func (d *MDNSDiscovery) handleEntry(entry *zeroconf.ServiceEntry) {
	if len(entry.AddrIPv4) == 0 && len(entry.AddrIPv6) == 0 {
		return
	}
	var host string
	if len(entry.AddrIPv4) > 0 {
		host = entry.AddrIPv4[0].String()
	} else {
		host = entry.AddrIPv6[0].String()
	}
	port := entry.Port

	desc, err := probeDescription(host, port)
	if err != nil || !desc.hasDXCluster() {
		return // not a dxcluster instance or unreachable
	}

	// Build an Instance that matches the public-registry shape so the picker
	// can use it directly.
	name := desc.Receiver.Name
	if name == "" {
		name = unescapeMDNS(entry.Instance)
	}
	if name == "" {
		name = fmt.Sprintf("%s:%d", host, port)
	}

	inst := Instance{
		ID:               fmt.Sprintf("local:%s:%d", host, port),
		Callsign:         desc.Receiver.Callsign,
		Name:             name,
		Location:         desc.Receiver.Location,
		Host:             host,
		Port:             port,
		TLS:              false,
		AvailableClients: desc.AvailableClients,
		MaxClients:       desc.MaxClients,
		IsOnline:         true,
	}

	key := fmt.Sprintf("%s:%d", host, port)
	d.mu.Lock()
	d.instances[key] = inst
	d.mu.Unlock()

	if d.onChange != nil {
		d.onChange()
	}
}

// Instances returns a snapshot of all confirmed local instances.
func (d *MDNSDiscovery) Instances() []Instance {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Instance, 0, len(d.instances))
	for _, v := range d.instances {
		out = append(out, v)
	}
	return out
}

// Stop cancels the mDNS browse.
func (d *MDNSDiscovery) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

// unescapeMDNS removes backslash escapes from an mDNS instance name.
func unescapeMDNS(name string) string {
	result := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		if name[i] == '\\' && i+1 < len(name) {
			i++
			result = append(result, name[i])
		} else {
			result = append(result, name[i])
		}
	}
	return string(result)
}
