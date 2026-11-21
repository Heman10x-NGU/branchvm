// Command authdemo is a small signup and login API backed by Postgres 16.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDSN     = "postgres://postgres@127.0.0.1:5432/authdemo?sslmode=disable"
	tokenTTL       = 24 * time.Hour
	minPasswordLen = 8
)

// App holds the HTTP handlers and their dependencies.
type App struct {
	store  *Store
	secret []byte
	now    func() time.Time
}

// NewApp builds an App.
func NewApp(store *Store, secret []byte) *App {
	return &App{store: store, secret: secret, now: time.Now}
}

// Routes returns the HTTP handler.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signup", a.handleSignup)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("GET /me", a.handleMe)
	return mux
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *App) handleSignup(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCreds(w, r)
	if !ok {
		return
	}
	if _, err := mail.ParseAddress(c.Email); err != nil || !strings.Contains(c.Email, "@") {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}
	if len(c.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "password too short")
		return
	}
	hash, err := hashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	id, err := a.store.CreateUser(r.Context(), c.Email, hash)
	if errors.Is(err, ErrEmailTaken) {
		writeErr(w, http.StatusConflict, "email already registered")
		return
	}
	if err != nil {
		log.Printf("signup: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "token": a.issueToken(id)})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeCreds(w, r)
	if !ok {
		return
	}
	u, err := a.store.UserByEmail(r.Context(), c.Email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("login: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err != nil || !checkPassword(u.PasswordHash, c.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": a.issueToken(u.ID)})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing token")
		return
	}
	id, err := a.verifyToken(tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	u, err := a.store.UserByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if err != nil {
		log.Printf("me: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "email": u.Email})
}

func decodeCreds(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return c, false
	}
	c.Email = NormalizeEmail(c.Email)
	return c, true
}

// issueToken returns "<id>.<expiry unix>.<hex hmac>".
func (a *App) issueToken(id int64) string {
	payload := strconv.FormatInt(id, 10) + "." + strconv.FormatInt(a.now().Add(tokenTTL).Unix(), 10)
	return payload + "." + a.sign(payload)
}

func (a *App) verifyToken(tok string) (int64, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, errors.New("malformed token")
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(a.sign(payload)), []byte(parts[2])) {
		return 0, errors.New("bad signature")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse expiry: %w", err)
	}
	if a.now().Unix() > exp {
		return 0, errors.New("token expired")
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id: %w", err)
	}
	return id, nil
}

func (a *App) sign(payload string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// hashPassword returns "<hex salt>$<hex sha256(salt|password)>".
// Demo-grade only; a production service would use bcrypt or argon2.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	return hex.EncodeToString(salt) + "$" + digest(salt, pw), nil
}

func checkPassword(stored, pw string) bool {
	saltHex, sum, ok := strings.Cut(stored, "$")
	if !ok {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(digest(salt, pw)), []byte(sum))
}

func digest(salt []byte, pw string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	return hex.EncodeToString(h.Sum(nil))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}
	secret := os.Getenv("AUTH_SECRET")
	if secret == "" {
		secret = "authdemo-dev-secret"
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	store, err := OpenStore(ctx, dsn)
	if err == nil {
		err = store.Migrate(ctx)
	}
	cancel()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	defer store.Close()
	srv := &http.Server{Addr: addr, Handler: NewApp(store, []byte(secret)).Routes(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("authdemo listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
