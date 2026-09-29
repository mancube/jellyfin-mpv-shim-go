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
