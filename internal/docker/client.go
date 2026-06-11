// Package docker provides a lightweight client for the Docker Engine API,
// communicating directly over the Unix socket without any SDK dependency.
// Only the endpoints dtk needs (container list, pause, unpause) are implemented.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// Client communicates with the Docker daemon over its Unix socket.
type Client struct {
	http       *http.Client
	apiBase    string // e.g. "http://localhost"
	socketPath string
}

// containerSummary is the subset of the Docker API /containers/json response
// that dtk cares about.
type containerSummary struct {
	Names []string `json:"Names"`
	State string   `json:"State"` // "running", "paused", "exited", …
}

// New creates a Client that talks to the Docker daemon via socketPath
// (e.g. "/var/run/docker.sock"). The Docker API version used is v1.41,
// which has been stable since Docker 20.10.
func New(socketPath string) *Client {
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		http:       &http.Client{Transport: transport},
		apiBase:    "http://localhost/v1.41",
		socketPath: socketPath,
	}
}

// Close is a no-op (kept for interface compatibility). The underlying
// http.Transport manages connection pooling automatically.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// ContainerStates returns a map of container name → Docker state string
// ("running", "paused", "exited", …) for the requested names.
// Names not found on the daemon are omitted from the result.
func (c *Client) ContainerStates(ctx context.Context, names []string) (map[string]string, error) {
	// Build a JSON filter for the Docker API: {"name":["foo","bar"]}
	nameList := make([]string, len(names))
	copy(nameList, names)
	filterBytes, _ := json.Marshal(map[string][]string{"name": nameList})

	apiURL := c.apiBase + "/containers/json?all=1&filters=" + url.QueryEscape(string(filterBytes))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("docker list: HTTP %d: %s", resp.StatusCode, body)
	}

	var containers []containerSummary
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("docker list decode: %w", err)
	}

	result := make(map[string]string, len(names))
	for _, ctr := range containers {
		for _, cname := range ctr.Names {
			// Docker prepends "/" to container names.
			if len(cname) > 0 && cname[0] == '/' {
				cname = cname[1:]
			}
			for _, want := range names {
				if cname == want {
					result[want] = ctr.State
				}
			}
		}
	}
	return result, nil
}

// PauseGroup pauses every container in names.
// Already-paused or missing containers are silently skipped.
func (c *Client) PauseGroup(ctx context.Context, names []string) error {
	states, err := c.ContainerStates(ctx, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if states[name] == "paused" || states[name] == "" {
			continue
		}
		if err := c.containerAction(ctx, name, "pause"); err != nil {
			return fmt.Errorf("pause %q: %w", name, err)
		}
	}
	return nil
}

// UnpauseGroup unpauses every container in names.
// Non-paused or missing containers are silently skipped.
func (c *Client) UnpauseGroup(ctx context.Context, names []string) error {
	states, err := c.ContainerStates(ctx, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if states[name] != "paused" {
			continue
		}
		if err := c.containerAction(ctx, name, "unpause"); err != nil {
			return fmt.Errorf("unpause %q: %w", name, err)
		}
	}
	return nil
}

// RestartGroup restarts every container in names.
// Missing containers are silently skipped.
func (c *Client) RestartGroup(ctx context.Context, names []string) error {
	states, err := c.ContainerStates(ctx, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if states[name] == "" {
			continue
		}
		if err := c.containerAction(ctx, name, "restart"); err != nil {
			return fmt.Errorf("restart %q: %w", name, err)
		}
	}
	return nil
}

// containerAction sends a POST to /containers/{name}/{action} (e.g. pause/unpause).
func (c *Client) containerAction(ctx context.Context, name, action string) error {
	url := c.apiBase + "/containers/" + name + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204 No Content is the success response for both pause and unpause.
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
