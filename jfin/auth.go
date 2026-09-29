package jfin

import (
	"context"
	"errors"
)

// AuthenticateByName logs in and stores the token on the client.
func (c *Client) Login(ctx context.Context, username, password string) (*LoginResponse, error) {
	var resp LoginResponse
	err := c.Post(ctx, "/Users/AuthenticateByName", map[string]string{
		"Username": username,
		"Pw":       password,
	}, &resp)
	if err != nil {
		return nil, err
	}
	if resp.AccessToken == "" {
		return nil, errors.New("jfin: server returned no access token")
	}
	c.Token = resp.AccessToken
	c.UserID = resp.User.ID
	c.Username = username
	return &resp, nil
}

// SupportedCommands mirrors upstream constants.CAPABILITIES (v2.10 plus the
// two commands v3 added); the web UI draws the remote panel from this list.
var SupportedCommands = []string{
	"MoveUp", "MoveDown", "MoveLeft", "MoveRight",
	"Select", "Back", "ToggleFullscreen", "GoHome", "GoToSettings", "GoToSearch",
	"TakeScreenshot", "VolumeUp", "VolumeDown", "ToggleMute",
	"SetAudioStreamIndex", "SetSubtitleStreamIndex",
	"Mute", "Unmute", "SetVolume",
	"DisplayContent", "Play", "Playstate", "PlayNext", "PlayMediaSource",
	"ToggleContextMenu",
}

// PostCapabilities registers our device's capabilities; sent on (re)connect
// (PLAN.md §2.6).
func (c *Client) PostCapabilities(ctx context.Context) error {
	body := map[string]any{
		"DeviceId":                     c.DeviceID,
		"ClientName":                   ClientName,
		"DeviceName":                   c.Device,
		"PlayableMediaTypes":           []string{"Video"},
		"SupportsMediaControl":         true,
		"SupportsPersistentIdentifier": true,
		"SupportedCommands":            SupportedCommands,
	}
	return c.Post(ctx, "/Sessions/Capabilities/Full", body, nil)
}

// Healthy reports whether the server still lists our session
// (GET /Sessions contains our DeviceId).
func (c *Client) Healthy(ctx context.Context) bool {
	var sessions []struct {
		DeviceID string `json:"DeviceId"`
	}
	if err := c.Get(ctx, "/Sessions", &sessions); err != nil {
		return false
	}
	for _, s := range sessions {
		if s.DeviceID == c.DeviceID {
			return true
		}
	}
	return false
}
