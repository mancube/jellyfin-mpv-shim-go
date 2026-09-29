package jfin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLoginAndHeaderAuth(t *testing.T) {
	var loginAuth, userAuth, userReqPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			loginAuth = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			var m map[string]string
			_ = json.Unmarshal(body, &m)
			if m["Username"] != "admin" || m["Pw"] != "s3cret" {
				t.Errorf("login body = %v, want admin/s3cret", m)
			}
			_, _ = w.Write([]byte(`{"AccessToken":"tok123","ServerName":"srv","User":{"Id":"user-1","Name":"Admin"}}`))
		case "/Users/user-1":
			userAuth = r.Header.Get("Authorization")
			userReqPath = r.URL.RequestURI()
			_, _ = w.Write([]byte(`{"Id":"user-1","Name":"Admin"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "player", "dev-uuid", "0.1.0", false)
	resp, err := c.Login(context.Background(), "admin", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if resp.AccessToken != "tok123" || c.Token != "tok123" || c.UserID != "user-1" {
		t.Fatalf("post-login state = %+v", c)
	}
	if strings.Contains(loginAuth, `Token="tok123"`) {
		t.Errorf("pre-login header leaked token: %s", loginAuth)
	}
	if !strings.Contains(loginAuth, `DeviceId="dev-uuid"`) || !strings.Contains(loginAuth, `Client="Jellyfin MPV Shim"`) {
		t.Errorf("auth header missing identity: %s", loginAuth)
	}

	var u User
	if err := c.Get(context.Background(), "/Users/user-1", &u); err != nil {
		t.Fatal(err)
	}
	if u.Name != "Admin" {
		t.Fatalf("user = %+v", u)
	}
	if !strings.Contains(userAuth, `Token="tok123"`) {
		t.Errorf("auth header missing token: %s", userAuth)
	}
	if strings.Contains(userReqPath, "api_key") || strings.Contains(userReqPath, "Token=") {
		t.Errorf("token in URL (legacy auth): %s", userReqPath)
	}
}

func TestCredStore(t *testing.T) {
	dir := t.TempDir()
	path := CredPath(dir)
	cf := &CredFile{}
	if err := cf.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := cf.ActiveAccount(); ok {
		t.Fatal("fresh store should be empty")
	}
	cf.Upsert(Account{Server: "http://a", Username: "u1", UserID: "id1", AccessToken: "t1"})
	cf.Upsert(Account{Server: "http://a", Username: "u2", UserID: "id2", AccessToken: "t2"})
	cf.Upsert(Account{Server: "http://a", Username: "u1", UserID: "id1b", AccessToken: "t1b"}) // replace
	if len(cf.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cf.Accounts))
	}
	if a, _ := cf.Get("http://a", "u1"); a.AccessToken != "t1b" {
		t.Errorf("upsert did not replace: %+v", a)
	}
	if err := cf.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cred file mode = %o, want 600", fi.Mode().Perm())
	}
	cf2 := &CredFile{}
	if err := cf2.Load(path); err != nil {
		t.Fatal(err)
	}
	if len(cf2.Accounts) != 2 {
		t.Fatalf("reload accounts = %d, want 2", len(cf2.Accounts))
	}
	if a, ok := cf2.ActiveAccount(); !ok || a.AccessToken != "t1b" {
		t.Errorf("active = %+v, want u1/t1b", a)
	}
	if !cf2.Remove("http://a", "u1") || cf2.Remove("http://a", "u1") {
		t.Error("remove should succeed once, then fail")
	}
	if a, ok := cf2.ActiveAccount(); !ok || a.Username != "u2" {
		t.Errorf("active after remove = %+v, want u2", a)
	}
	if err := cf2.Save(path); err != nil {
		t.Fatal(err)
	}
}

func TestNewNormalizesBase(t *testing.T) {
	c := New("localhost:8096/", "d", "u", "1", false)
	if c.Base != "http://localhost:8096" {
		t.Errorf("base = %q", c.Base)
	}
	c2 := New("https://jellyfin.home/", "d", "u", "1", false)
	if c2.Base != "https://jellyfin.home" {
		t.Errorf("base = %q", c2.Base)
	}
	dead := New("127.0.0.1:1", "d", "u", "1", false)
	if err := dead.Get(context.Background(), "/x", nil); err == nil {
		t.Error("expected error for unreachable host")
	}
}

func TestQuickConnectFlow(t *testing.T) {
	var exchanges int
	mux := http.NewServeMux()
	mux.HandleFunc("/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("true"))
	})
	mux.HandleFunc("/QuickConnect/Initiate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(QuickConnect{Secret: "sec", Code: "ABCD"})
	})
	mux.HandleFunc("/QuickConnect/Exchange", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("code") != "ABCD" || r.URL.Query().Get("Secret") != "sec" {
			t.Errorf("bad query: %v", r.URL.Query())
		}
		exchanges++
		if exchanges < 3 {
			w.WriteHeader(400) // not authorized yet
			return
		}
		_ = json.NewEncoder(w).Encode(LoginResponse{
			AccessToken: "tok", User: User{ID: "u1", Name: "bob"},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := New(ts.URL, "dev", "d", "1.0", false)
	ctx := context.Background()
	if !c.QuickConnectEnabled(ctx) {
		t.Fatal("QuickConnectEnabled = false")
	}
	qc, err := c.QuickConnectInitiate(ctx)
	if err != nil || qc.Code != "ABCD" {
		t.Fatalf("Initiate = %+v, %v", qc, err)
	}
	var got *LoginResponse
	for i := 0; i < 3; i++ {
		resp, ok, err := c.QuickConnectExchange(ctx, qc)
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if ok {
			got = resp
			break
		}
	}
	if got == nil || got.AccessToken != "tok" {
		t.Fatalf("Exchange never returned credentials: %+v", got)
	}
	if c.Token != "tok" || c.UserID != "u1" {
		t.Errorf("client not authenticated: token=%q user=%q", c.Token, c.UserID)
	}
}
