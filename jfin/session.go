package jfin

import (
	"context"
	"net/url"
)

// Session playback reports and teardown (port of the apiclient session
// methods used by the player).

// SessionPlaying sends the start-of-playback report.
func (c *Client) SessionPlaying(ctx context.Context, info *SessionInfo) error {
	return c.Post(ctx, "/Sessions/Playing", info, nil)
}

// SessionProgress sends a playback progress update.
func (c *Client) SessionProgress(ctx context.Context, info *SessionInfo) error {
	return c.Post(ctx, "/Sessions/Playing/Progress", info, nil)
}

// SessionStopped sends the end-of-playback report.
func (c *Client) SessionStopped(ctx context.Context, info *SessionInfo) error {
	return c.Post(ctx, "/Sessions/Playing/Stopped", info, nil)
}

// SetPlayed marks an item watched (or un-watched). Port of upstream
// set_played (the force_set_played default is off, so our player marks
// explicitly at ≥90% and on clean end).
func (c *Client) SetPlayed(ctx context.Context, itemID string, watched bool) error {
	path := "/Users/" + c.UserID + "/PlayedItems/" + url.PathEscape(itemID)
	if watched {
		return c.Post(ctx, path, nil, nil)
	}
	return c.Delete(ctx, path)
}

// CloseTranscode terminates a transcode session
// (DELETE /Videos/ActiveEncodings). Port of upstream terminate_transcode's
// non-live path.
func (c *Client) CloseTranscode(ctx context.Context, playSessionID string) error {
	return c.Delete(ctx, "/Videos/ActiveEncodings?DeviceId="+url.PathEscape(c.DeviceID)+
		"&PlaySessionId="+url.PathEscape(playSessionID))
}

// CloseLiveStream closes a live stream (live TV is out of scope, but the
// teardown call is here for parity with upstream).
func (c *Client) CloseLiveStream(ctx context.Context, liveStreamID string) error {
	return c.Post(ctx, "/LiveStreams/Close?liveStreamId="+url.PathEscape(liveStreamID), nil, nil)
}
