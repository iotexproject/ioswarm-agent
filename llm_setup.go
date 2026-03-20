package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Claude OAuth constants (same as CLIProxyAPI / Claude Code)
const (
	claudeAuthURL   = "https://claude.ai/oauth/authorize"
	claudeTokenURL  = "https://api.anthropic.com/v1/oauth/token"
	claudeClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeRedirect  = "http://localhost:54545/callback"
	claudeScope     = "org:create_api_key user:profile user:inference"
	claudeTokenFile = "claude_token.json"
)

type claudeToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	Expire       string `json:"expire"`
	OrgID        string `json:"org_id"`
	Type         string `json:"type"`
}

type pkceCodes struct {
	Verifier  string
	Challenge string
}

func generatePKCE() *pkceCodes {
	b := make([]byte, 32)
	rand.Read(b)
	verifier := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	return &pkceCodes{Verifier: verifier, Challenge: challenge}
}

func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}

func tokenDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".ioswarm")
	os.MkdirAll(dir, 0700)
	return dir
}

func tokenPath() string {
	return filepath.Join(tokenDir(), claudeTokenFile)
}

func loadToken() (*claudeToken, error) {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return nil, err
	}
	var t claudeToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func saveToken(t *claudeToken) error {
	t.Type = "claude"
	data, _ := json.MarshalIndent(t, "", "  ")
	return os.WriteFile(tokenPath(), data, 0600)
}

// runClaudeOAuth runs the full OAuth flow:
// 1. Start local HTTP server on :54545
// 2. Open browser to Claude OAuth page
// 3. User logs in → redirect back with code
// 4. Exchange code for access_token + refresh_token
// 5. Save tokens to ~/.ioswarm/claude_token.json
func runClaudeOAuth() (*claudeToken, error) {
	pkce := generatePKCE()
	state := generateState()

	// Build auth URL
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeClientID},
		"response_type":         {"code"},
		"redirect_uri":          {claudeRedirect},
		"scope":                 {claudeScope},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	authURL := claudeAuthURL + "?" + params.Encode()

	// Channel to receive the OAuth callback
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	// Start local callback server
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		cbState := r.URL.Query().Get("state")

		if cbState != state {
			errCh <- fmt.Errorf("state mismatch")
			w.Write([]byte("Authentication failed: state mismatch. Close this tab."))
			return
		}
		if code == "" {
			errCh <- fmt.Errorf("no code in callback")
			w.Write([]byte("Authentication failed: no code. Close this tab."))
			return
		}

		codeCh <- code
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body style="background:#111;color:#fff;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
			<div style="text-align:center">
				<h1>🐝 Authenticated!</h1>
				<p>You can close this tab and return to your terminal.</p>
			</div></body></html>`))
	})

	listener, err := net.Listen("tcp", ":54545")
	if err != nil {
		return nil, fmt.Errorf("cannot start callback server on :54545: %w", err)
	}

	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())

	// Open browser
	fmt.Println("Opening browser for Claude login...")
	fmt.Printf("If browser doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	// Wait for callback
	fmt.Println("Waiting for authentication...")
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("authentication timed out (5 min)")
	}

	// Exchange code for tokens
	fmt.Println("Exchanging code for tokens...")
	tokenParams := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {claudeClientID},
		"code":          {code},
		"redirect_uri":  {claudeRedirect},
		"code_verifier": {pkce.Verifier},
	}

	resp, err := http.Post(claudeTokenURL, "application/x-www-form-urlencoded", strings.NewReader(tokenParams.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body := make([]byte, 500)
		n, _ := resp.Body.Read(body)
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(body[:n]))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
		Account struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	token := &claudeToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        tokenResp.Account.EmailAddress,
		OrgID:        tokenResp.Organization.UUID,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	if err := saveToken(token); err != nil {
		return nil, fmt.Errorf("failed to save token: %w", err)
	}

	return token, nil
}

// runLLMSetup is the "ioswarm-agent llm setup" subcommand
func runLLMSetup() {
	fmt.Println("🐝 ClawHive LLM Setup")
	fmt.Println()

	// Check for existing token
	if existing, err := loadToken(); err == nil && existing.AccessToken != "" {
		fmt.Printf("Found existing Claude token for %s\n", existing.Email)
		fmt.Printf("Expires: %s\n", existing.Expire)
		fmt.Print("Re-authenticate? [y/N]: ")
		var answer string
		fmt.Scanln(&answer)
		if strings.ToLower(answer) != "y" {
			fmt.Println("Using existing token.")
			fmt.Printf("Start LLM proxy: ioswarm-agent --mode llm\n")
			return
		}
	}

	// Run OAuth
	token, err := runClaudeOAuth()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Authentication failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("✅ Authenticated as %s\n", token.Email)
	fmt.Printf("   Token saved to %s\n", tokenPath())
	fmt.Printf("   Expires: %s\n", token.Expire)
	fmt.Println()
	fmt.Println("Start LLM proxy:")
	fmt.Println("  ioswarm-agent --mode llm")
	fmt.Println()
	fmt.Println("Or run both validator + LLM:")
	fmt.Println("  ioswarm-agent --mode both --agent-id YOUR_ID")
}
