package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/handler"
)

// TestEnvironmentOutranksStoredSettingsAcrossRedeploys walks the deployment story that
// motivated the precedence rule: an operator runs the container, changes an environment
// variable, redeploys against the same volume, and expects the value they just set.
//
// Under the old precedence they got the opposite, silently. The stored row won, and
// because boot seeded environment SMTP into that row on first start, the row was never
// empty again — so every later change to EMAIL_SMTP_HOST was a no-op with one log line to
// show for it. Each stage below is one step of that story.
//
// The live mailer is observed through the SMTP relay: EMAIL_SMTP_CONNECT_HOST redirects
// the dial to a TLS listener here, which records the SNI name. That name is the host the
// running mailer actually holds, not merely what a resolver returned.
func TestEnvironmentOutranksStoredSettingsAcrossRedeploys(t *testing.T) {
	names := make(chan string, 8)
	relay := httptest.NewUnstartedServer(http.NotFoundHandler())
	relay.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		names <- hello.ServerName
		return nil, errors.New("test rejects handshake")
	}}
	relay.StartTLS()
	defer relay.Close()
	relayHost, relayPort, err := net.SplitHostPort(relay.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	apiKey := setupOwner(t, database, logger)

	// boot stands in for one container start: wire everything from a Config the way main
	// does, hand back the handler, and tear the worker down afterwards.
	boot := func(t *testing.T, envSMTPHost string) *handler.Handler {
		t.Helper()
		cfg := &config.Config{
			BaseURL: "https://calnode.localhost", DataDir: t.TempDir(),
			SMTPConnectHost: relayHost, SMTPConnectPort: relayPort,
		}
		if envSMTPHost != "" {
			cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPTLS = envSMTPHost, "465", true
			cfg.EmailFrom = "env@example.com"
		}
		ctx, cancel := context.WithCancel(t.Context())
		h, drain := BuildHandler(ctx, cfg, database, logger)
		t.Cleanup(func() { cancel(); drain() })
		return h
	}

	// dialedHost sends the admin test email and reports the host the mailer dialed.
	dialedHost := func(t *testing.T, h *handler.Handler) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/settings/email/test", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.TestEmailConnection)(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("test email: %d %s (the relay always rejects the handshake)", rec.Code, rec.Body)
		}
		select {
		case name := <-names:
			return name
		case <-time.After(2 * time.Second):
			t.Fatal("test email never reached the relay")
			return ""
		}
	}

	// Stage 1 — first deploy. EMAIL_SMTP_HOST is set and the database is empty.
	h := boot(t, "smtp.first.example")
	if got := dialedHost(t, h); got != "smtp.first.example" {
		t.Fatalf("first deploy dialed %q; want smtp.first.example", got)
	}

	// The environment must not be copied into server_settings. That seeding is what used
	// to make the row permanently non-empty and every later environment change a no-op;
	// with the environment winning outright there is nothing to seed and nothing to shadow.
	if stored := storedSMTPHost(t, database); stored != "" {
		t.Errorf("server_settings.smtp_host = %q after an environment-only boot; want it left empty "+
			"(seeding it is what made later environment changes silently ineffective)", stored)
	}

	// Stage 2 — THE REDEPLOY. Same volume, same database, EMAIL_SMTP_HOST changed.
	h = boot(t, "smtp.second.example")
	if got := dialedHost(t, h); got != "smtp.second.example" {
		t.Fatalf("after redeploying with a changed EMAIL_SMTP_HOST the mailer dialed %q; "+
			"want smtp.second.example", got)
	}

	// Stage 3 — an admin saves different settings in the UI while the variable is still
	// set. The row is written (it is the fallback, and must stay editable), but the
	// environment keeps delivering. Before, this save would have captured the mailer.
	patchEmailHost(t, h, apiKey, "smtp.ui.example")
	if stored := storedSMTPHost(t, database); stored != "smtp.ui.example" {
		t.Errorf("server_settings.smtp_host = %q; want the save to persist as the fallback", stored)
	}
	if got := dialedHost(t, h); got != "smtp.second.example" {
		t.Errorf("after saving in the admin UI the mailer dialed %q; want smtp.second.example — "+
			"a save must not dislodge the environment until the next restart", got)
	}

	// Stage 4 — redeploy once more, now that BOTH sources are populated. This is the
	// state a real instance reaches as soon as anyone opens the settings page, and the
	// one the old code resolved the wrong way round.
	logs.Reset()
	h = boot(t, "smtp.second.example")
	if got := dialedHost(t, h); got != "smtp.second.example" {
		t.Errorf("with both sources populated the mailer dialed %q; want the environment's "+
			"smtp.second.example", got)
	}
	// And it has to say so. An override nobody can see is a quieter version of the old
	// confusion: the settings page goes on showing values that are not delivering mail.
	if !strings.Contains(logs.String(), "so Settings → Email is ignored") {
		t.Error("boot did not warn that the environment was overriding stored email settings")
	}

	// Stage 5 — the variable is removed. The stored row is the fallback, so the value
	// saved in stage 3 takes over. This is the escape hatch: unset, redeploy, UI is live.
	// Nothing else has to be cleaned up — the row was never overwritten.
	h = boot(t, "")
	if got := dialedHost(t, h); got != "smtp.ui.example" {
		t.Errorf("with EMAIL_SMTP_HOST unset the mailer dialed %q; want the stored smtp.ui.example", got)
	}
}

// setupOwner creates the first user and returns their API key.
func setupOwner(t *testing.T, database *sql.DB, logger *slog.Logger) string {
	t.Helper()
	seed := handler.New(database, logger)
	rec := httptest.NewRecorder()
	seed.Setup(rec, httptest.NewRequest(http.MethodPost, "/v1/setup",
		strings.NewReader(`{"name":"Host","email":"host@example.com","timezone":"UTC"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	var identity struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	return identity.APIKey
}

func patchEmailHost(t *testing.T, h *handler.Handler, apiKey, smtpHost string) {
	t.Helper()
	body := fmt.Sprintf(`{"smtp_host":%q,"smtp_port":"465","smtp_tls":true,"email_from":"ui@example.com"}`, smtpHost)
	req := httptest.NewRequest(http.MethodPatch, "/v1/settings/email", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEmailSettings)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch email settings: %d %s", rec.Code, rec.Body)
	}
}

func storedSMTPHost(t *testing.T, database *sql.DB) string {
	t.Helper()
	var host string
	if err := database.QueryRow(`SELECT smtp_host FROM server_settings WHERE id = 1`).Scan(&host); err != nil {
		t.Fatalf("read stored smtp_host: %v", err)
	}
	return host
}

// TestEnvironmentOutranksStoredOAuthApps covers the same rule for the Google and Zoom
// OAuth apps, which have no seeding step but did have the same "stored row wins" boot
// order. The observable is the resolved app rather than a network dial: these credentials
// are handed to provider clients, not to a socket this test can intercept.
func TestEnvironmentOutranksStoredOAuthApps(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	apiKey := setupOwner(t, database, logger)

	cfg := &config.Config{
		BaseURL: "https://calnode.localhost", DataDir: t.TempDir(),
		GoogleClientID: "env-google-id", GoogleClientSecret: "env-google-secret",
		ZoomClientID: "env-zoom-id", ZoomClientSecret: "env-zoom-secret",
	}
	ctx, cancel := context.WithCancel(t.Context())
	h, drain := BuildHandler(ctx, cfg, database, logger)
	t.Cleanup(func() { cancel(); drain() })

	for _, c := range []struct {
		name, path, envVar, storedID string
	}{
		{"google", "/v1/settings/google", "GOOGLE_CLIENT_ID", "stored-google-id"},
		{"zoom", "/v1/settings/zoom", "ZOOM_CLIENT_ID", "stored-zoom-id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Save a different app in the UI. It persists as the fallback...
			body := fmt.Sprintf(`{"client_id":%q,"client_secret":"stored-secret"}`, c.storedID)
			req := httptest.NewRequest(http.MethodPatch, c.path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			switch c.name {
			case "google":
				h.RequireAuth(h.PatchGoogleSettings)(rec, req)
			default:
				h.RequireAuth(h.PatchZoomSettings)(rec, req)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("patch %s: %d %s", c.name, rec.Code, rec.Body)
			}

			// ...and the response says plainly that the environment is in charge, so the
			// page can tell an admin why their save is not the app in use.
			var got struct {
				ClientID string `json:"client_id"`
				Env      struct {
					Managed  bool   `json:"managed"`
					ClientID string `json:"client_id"`
				} `json:"env"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ClientID != c.storedID {
				t.Errorf("stored client_id = %q; want %q saved as the fallback", got.ClientID, c.storedID)
			}
			if !got.Env.Managed || got.Env.ClientID == "" {
				t.Errorf("env block = %+v; want it to report that %s is in charge", got.Env, c.envVar)
			}
		})
	}

	// And a fresh boot still resolves to the environment app, not the rows just saved.
	dbGoogle, err := handler.LoadGoogleSettingsFromDB(database, [32]byte{})
	if err != nil {
		t.Fatalf("load stored google settings: %v", err)
	}
	if g := handler.MergeGoogleSettings(h.EnvSettings(), dbGoogle); g == nil || g.ClientID != "env-google-id" {
		t.Errorf("resolved google app = %+v; want the environment's env-google-id", g)
	}
}
