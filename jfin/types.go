package jfin

// User is the subset of the user object we use.
type User struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// LoginResponse is the /Users/AuthenticateByName payload.
type LoginResponse struct {
	AccessToken string `json:"AccessToken"`
	User        User   `json:"User"`
	ServerName  string `json:"ServerName"`
}
