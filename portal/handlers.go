package main

import (
	"context"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type contextKey int

const userIDKey contextKey = 0

func setUserID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

func getUserID(ctx context.Context) int64 {
	id, _ := ctx.Value(userIDKey).(int64)
	return id
}

// App holds the application state.
type App struct {
	store        *Store
	masterSecret string
	templates    *template.Template
	swarmAPI     string // base URL for coordinator's SwarmAPI
	httpClient   *http.Client
}

// NewApp creates a new App with parsed templates.
func NewApp(store *Store, masterSecret, templatesDir, swarmAPI string) *App {
	tmpl := template.Must(template.ParseGlob(templatesDir + "/*.html"))
	return &App{
		store:        store,
		masterSecret: masterSecret,
		templates:    tmpl,
		swarmAPI:     strings.TrimRight(swarmAPI, "/"),
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Routes registers all HTTP routes.
func (app *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /register", app.registerForm)
	mux.HandleFunc("POST /register", app.registerSubmit)
	mux.HandleFunc("GET /login", app.loginForm)
	mux.HandleFunc("POST /login", app.loginSubmit)
	mux.HandleFunc("POST /logout", app.sessionMiddleware(app.logout))

	mux.HandleFunc("GET /dashboard", app.sessionMiddleware(app.dashboard))
	mux.HandleFunc("GET /api-keys", app.sessionMiddleware(app.apiKeysList))
	mux.HandleFunc("POST /api-keys/create", app.sessionMiddleware(app.apiKeysCreate))
	mux.HandleFunc("POST /api-keys/revoke", app.sessionMiddleware(app.apiKeysRevoke))

	// SwarmAPI proxy — forwards /api/swarm/* to coordinator's SwarmAPI
	mux.HandleFunc("GET /api/swarm/", app.sessionMiddleware(app.swarmProxy))

	// Redirect root to dashboard (or login)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})

	return mux
}

func (app *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// --- Registration ---

func (app *App) registerForm(w http.ResponseWriter, r *http.Request) {
	app.render(w, "register.html", map[string]string{})
}

func (app *App) registerSubmit(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	if email == "" || password == "" {
		app.render(w, "register.html", map[string]string{"Error": "Email and password required"})
		return
	}
	if len(password) < 8 {
		app.render(w, "register.html", map[string]string{"Error": "Password must be at least 8 characters"})
		return
	}

	hash, err := HashPassword(password)
	if err != nil {
		app.render(w, "register.html", map[string]string{"Error": "Internal error"})
		return
	}

	_, err = app.store.CreateUser(email, hash)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			app.render(w, "register.html", map[string]string{"Error": "Email already registered"})
		} else {
			app.render(w, "register.html", map[string]string{"Error": "Internal error"})
		}
		return
	}

	http.Redirect(w, r, "/login?registered=1", http.StatusSeeOther)
}

// --- Login ---

func (app *App) loginForm(w http.ResponseWriter, r *http.Request) {
	data := map[string]string{}
	if r.URL.Query().Get("registered") == "1" {
		data["Success"] = "Account created. Please login."
	}
	app.render(w, "login.html", data)
}

func (app *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	user, err := app.store.GetUserByEmail(email)
	if err != nil {
		app.render(w, "login.html", map[string]string{"Error": "Invalid email or password"})
		return
	}
	if !CheckPassword(user.PasswordHash, password) {
		app.render(w, "login.html", map[string]string{"Error": "Invalid email or password"})
		return
	}

	token, err := GenerateSessionToken()
	if err != nil {
		app.render(w, "login.html", map[string]string{"Error": "Internal error"})
		return
	}

	if err := app.store.CreateSession(token, user.ID); err != nil {
		app.render(w, "login.html", map[string]string{"Error": "Internal error"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7, // 7 days
	})

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// --- Logout ---

func (app *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		app.store.DeleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Dashboard ---

func (app *App) dashboard(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r.Context())
	keyCount, _ := app.store.CountActiveKeys(userID)

	data := map[string]any{
		"KeyCount": keyCount,
	}
	app.render(w, "dashboard.html", data)
}

// --- API Keys ---

func (app *App) apiKeysList(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r.Context())
	keys, err := app.store.ListAPIKeys(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Check if a newly created key should be shown
	newKey := r.URL.Query().Get("new")

	data := map[string]any{
		"Keys":   keys,
		"NewKey": newKey,
	}
	app.render(w, "api_keys.html", data)
}

func (app *App) apiKeysCreate(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r.Context())
	label := strings.TrimSpace(r.FormValue("label"))
	agentID := strings.TrimSpace(r.FormValue("agent_id"))

	if agentID == "" {
		agentID = GenerateAgentID()
	}

	apiKey := DeriveAgentToken(app.masterSecret, agentID)

	_, err := app.store.CreateAPIKey(userID, agentID, apiKey, label)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Redirect(w, r, "/api-keys?error=agent_id+already+exists", http.StatusSeeOther)
		} else {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	http.Redirect(w, r, "/api-keys?new="+url.QueryEscape(apiKey), http.StatusSeeOther)
}

func (app *App) apiKeysRevoke(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r.Context())
	keyIDStr := r.FormValue("key_id")
	keyID, err := strconv.ParseInt(keyIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid key ID", http.StatusBadRequest)
		return
	}

	if err := app.store.RevokeAPIKey(userID, keyID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/api-keys", http.StatusSeeOther)
}

// --- SwarmAPI Proxy ---

// swarmProxy forwards requests from /api/swarm/* to the coordinator's SwarmAPI.
// E.g., /api/swarm/status → http://coordinator:14690/swarm/status
func (app *App) swarmProxy(w http.ResponseWriter, r *http.Request) {
	// Map /api/swarm/status → /swarm/status
	path := strings.TrimPrefix(r.URL.Path, "/api")
	targetURL := app.swarmAPI + path

	resp, err := app.httpClient.Get(targetURL)
	if err != nil {
		http.Error(w, `{"error":"coordinator unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

