package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// instancesURL is the public UberSDR instance directory. Every registered
// receiver reports itself here along with the list of add-ons it runs.
const instancesURL = "https://instances.ubersdr.org/api/instances"

// dxclusterAddon is the add-on name we filter on.
const dxclusterAddon = "dxcluster"

// Instance is one UberSDR receiver as reported by the instance directory.
// Only the fields this client needs are decoded.
type Instance struct {
	ID               string   `json:"id"`
	Callsign         string   `json:"callsign"`
	Name             string   `json:"name"`
	Location         string   `json:"location"`
	Version          string   `json:"version"`
	PublicURL        string   `json:"public_url"`
	Host             string   `json:"host"`
	Port             int      `json:"port"`
	TLS              bool     `json:"tls"`
	MaxClients       int      `json:"max_clients"`
	AvailableClients int      `json:"available_clients"`
	Addons           []string `json:"addons"`
	IsOnline         bool     `json:"is_online"`
}

type instancesResponse struct {
	Count     int        `json:"count"`
	Instances []Instance `json:"instances"`
}

// HasDXCluster reports whether the instance advertises the DX cluster add-on.
func (i Instance) HasDXCluster() bool {
	for _, a := range i.Addons {
		if strings.EqualFold(a, dxclusterAddon) {
			return true
		}
	}
	return false
}

// TerminalWSURL builds the WebSocket URL of the instance's DX cluster terminal.
//
// The add-on is reached through UberSDR's reverse proxy at
// /addon/dxcluster/, and its WebSocket terminal lives at /api/terminal —
// so the full path is /addon/dxcluster/api/terminal. TLS instances use wss.
func (i Instance) TerminalWSURL() string {
	scheme := "ws"
	if i.TLS {
		scheme = "wss"
	}
	hostport := i.Host
	if i.Port != 0 {
		hostport = fmt.Sprintf("%s:%d", i.Host, i.Port)
	}
	return fmt.Sprintf("%s://%s/addon/dxcluster/api/terminal", scheme, hostport)
}

// HTTPURL builds the plain HTTP(S) base URL of the instance, suitable for
// handing to the ubersdr-audio client's POST /api/v1/connect endpoint.
//
// If the directory supplied a public_url it is used verbatim (trailing slash
// trimmed); otherwise the URL is composed from host/port/tls.
func (i Instance) HTTPURL() string {
	if u := strings.TrimSpace(i.PublicURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "http"
	if i.TLS {
		scheme = "https"
	}
	hostport := i.Host
	if i.Port != 0 {
		hostport = fmt.Sprintf("%s:%d", i.Host, i.Port)
	}
	return fmt.Sprintf("%s://%s", scheme, hostport)
}

// FetchDXClusterInstances fetches the instance directory and returns only the
// instances that run the DX cluster add-on, sorted by name.
func FetchDXClusterInstances(ctx context.Context) ([]Instance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, instancesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instance directory returned HTTP %d", resp.StatusCode)
	}

	var data instancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode instance directory: %w", err)
	}

	out := make([]Instance, 0, len(data.Instances))
	for _, inst := range data.Instances {
		if inst.HasDXCluster() {
			out = append(out, inst)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out, nil
}
