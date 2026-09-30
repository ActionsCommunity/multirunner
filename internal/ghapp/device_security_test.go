package ghapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceBaseURLSecurity(t *testing.T) {
	for _, raw := range []string{"http://github.com", "http://www.github.com/", "https://github.com"} {
		got, err := DeviceBaseURL(raw)
		if err != nil || got != DefaultBaseURL {
			t.Fatalf("%s: got=%s err=%v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://github.example.com", "ftp://github.example.com", "https://user:secret@github.example.com", "http://localhost"} {
		if _, err := NewTokenRefresher("cid", raw, "unused-path"); err == nil || !strings.Contains(err.Error(), "URL") {
			t.Fatalf("unsafe endpoint accepted: %s: %v", raw, err)
		}
	}
}

func TestOAuthRefreshRefusesCrossOriginRedirect(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	if _, err := RefreshUserToken(context.Background(), "cid", source.URL, "refresh-secret"); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked {
		t.Fatal("OAuth request reached another origin")
	}
}

func TestInstallationRepositorySelection(t *testing.T) {
	for _, included := range []bool{true, false} {
		t.Run(fmt.Sprint(included), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/user/installations/7/repositories" || r.Header.Get("Authorization") != "Bearer user-token" {
					t.Errorf("wrong request: %s", r.URL.Path)
				}
				if r.URL.Query().Get("page") == "1" {
					_, _ = w.Write([]byte(`{"repositories":[`))
					for i := 0; i < 100; i++ {
						if i > 0 {
							fmt.Fprint(w, ",")
						}
						fmt.Fprintf(w, `{"full_name":"acme/other%d"}`, i)
					}
					fmt.Fprint(w, ` ]}`)
				} else if included {
					fmt.Fprint(w, `{"repositories":[{"full_name":"ACME/Target"}]}`)
				} else {
					fmt.Fprint(w, `{"repositories":[]}`)
				}
			}))
			defer srv.Close()
			err := CheckInstallationRepository(context.Background(), srv.URL, "user-token", 7, "acme", "target")
			if (err == nil) != included {
				t.Fatalf("included=%v err=%v", included, err)
			}
		})
	}
}
