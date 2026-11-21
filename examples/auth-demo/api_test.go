package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	return NewApp(testStore(t), []byte("test-secret"))
}

func call(t *testing.T, a *App, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func signup(t *testing.T, a *App, email, pw string) string {
	t.Helper()
	code, out := call(t, a, "POST", "/signup", `{"email":"`+email+`","password":"`+pw+`"}`, "")
	if code != http.StatusCreated {
		t.Fatalf("signup %s: %d %v", email, code, out)
	}
	tok, _ := out["token"].(string)
	return tok
}

func TestSignupHandler(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"valid", `{"email":"new@example.com","password":"longenough"}`, 201},
		{"bad email", `{"email":"nope","password":"longenough"}`, 400},
		{"short password", `{"email":"x@example.com","password":"short"}`, 400},
		{"bad json", `{"email":`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := call(t, a, "POST", "/signup", tc.body, ""); code != tc.want {
				t.Fatalf("status %d, want %d (%v)", code, tc.want, out)
			}
		})
	}
}

func TestSignupDuplicateHandler(t *testing.T) {
	a := testApp(t)
	signup(t, a, "twice@example.com", "password1")
	code, _ := call(t, a, "POST", "/signup", `{"email":"twice@example.com","password":"password2"}`, "")
	if code != http.StatusConflict {
		t.Fatalf("status %d, want 409", code)
	}
}

func TestLogin(t *testing.T) {
	a := testApp(t)
	signup(t, a, "login@example.com", "correct-pw")
	cases := []struct {
		name, body string
		want       int
	}{
		{"correct", `{"email":"login@example.com","password":"correct-pw"}`, 200},
		{"wrong password", `{"email":"login@example.com","password":"wrong-pw"}`, 401},
		{"unknown user", `{"email":"ghost@example.com","password":"correct-pw"}`, 401},
		{"bad json", `nope`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := call(t, a, "POST", "/login", tc.body, "")
			if code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if tc.want == 200 && out["token"] == "" {
				t.Fatal("missing token")
			}
		})
	}
}

func TestMe(t *testing.T) {
	a := testApp(t)
	tok := signup(t, a, "me@example.com", "password1")
	cases := []struct {
		name, token string
		want        int
	}{
		{"valid", tok, 200},
		{"missing", "", 401},
		{"tampered", tok + "00", 401},
		{"garbage", "a.b", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := call(t, a, "GET", "/me", "", tc.token)
			if code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if tc.want == 200 && out["email"] != "me@example.com" {
				t.Fatalf("email %v", out["email"])
			}
		})
	}
}

func TestTokenRoundTrip(t *testing.T) {
	a := testApp(t)
	for _, id := range []int64{1, 42, 1 << 40} {
		got, err := a.verifyToken(a.issueToken(id))
		if err != nil || got != id {
			t.Fatalf("id %d: got %d err %v", id, got, err)
		}
	}
}

func TestTokenExpired(t *testing.T) {
	a := testApp(t)
	base := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return base }
	tok := a.issueToken(7)
	a.now = func() time.Time { return base.Add(tokenTTL + time.Second) }
	if _, err := a.verifyToken(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}
