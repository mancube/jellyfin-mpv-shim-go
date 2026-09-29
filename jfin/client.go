// Package jfin is the minimal Jellyfin client surface mpv-shim needs:
// REST over the non-legacy MediaBrowser auth header (tokens never appear in
// URLs), plus — from M1 on — the /socket websocket.
package jfin

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClientName is what the web UI shows as the device client; kept identical to
// upstream so UI affordances behave the same (PLAN.md §2.10).
const ClientName = "Jellyfin MPV Shim"

// Client is a small REST client for one Jellyfin server.
type Client struct {
	Base     string // e.g. https://host:8096 (no trailing slash)
	Device   string // player name, shown in the UI
	DeviceID string // stable per-install UUID
	Version  string // app version
	Token    string // empty pre-login
	UserID   string // set after login
	Username string // set after login

	http *http.Client
}

func New(base, device, deviceID, version string, ignoreSSL bool) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	tr := &http.Transport{}
	if ignoreSSL {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 — user opt-in
	}
	return &Client{
		Base: base, Device: device, DeviceID: deviceID, Version: version,
		http: &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
}

// AuthHeader builds the non-legacy MediaBrowser authorization header used on
// every request (REST and WS). The legacy query token is dead by default in
// v12, so this is the only auth mechanism.
func (c *Client) AuthHeader() string {
	return fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s", Token="%s"`,
		ClientName, c.Device, c.DeviceID, c.Version, c.Token)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if c.Base == "" {
		return errors.New("jfin: no server configured")
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.AuthHeader())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if out != nil {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jfin: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.do(ctx, http.MethodPost, path, in, out)
}

func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
