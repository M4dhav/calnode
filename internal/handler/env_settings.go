package handler

import "context"

// EnvSettings is the subset of process-environment configuration that server_settings
// can ALSO hold and the admin UI can ALSO edit. Three groups overlap that way: the email
// transport, the Google OAuth app and the Zoom OAuth app.
//
// # Precedence: the environment wins, and the stored row is the fallback
//
// An operator who sets a variable and redeploys must get the value they set. Before, the
// stored row won and the variable was ignored -- and because the boot path seeded env SMTP
// into the row on first boot, the row was never empty again, so EVERY later change to
// EMAIL_SMTP_HOST did nothing, with one log line as the only sign. That is the bug this
// type exists to make impossible.
//
// So: a value present in the environment is used. A value absent from the environment
// falls back to server_settings, and failing that to a built-in default. "Absent" is the
// empty string, which is why config.Load deliberately no longer applies its own defaults
// to these fields -- a default there would be indistinguishable from an operator's value
// and would override the stored row on every boot.
//
// # Why some fields resolve as a group
//
// Not every field stands alone. A client secret is meaningless beside a different client
// ID, and a password is meaningless beside a different host. Resolving those field by
// field would silently assemble a credential that half came from the environment and half
// from the database -- a thing no operator ever asked for, and which fails at the consent
// screen or the SMTP handshake rather than at boot. So fields that only mean something
// together resolve together, keyed on the one variable that names the thing:
//
//   - Email transport, keyed on EMAIL_SMTP_HOST or EMAIL_RESEND_API_KEY. Setting either
//     makes the environment the sole source of host, port, user, password, TLS mode and
//     API key. This is also what lets the environment pick the TRANSPORT: a stored Resend
//     key would otherwise still win at BuildMailer and route past the SMTP host that was
//     just set.
//   - Google app, keyed on GOOGLE_CLIENT_ID.
//   - Zoom app, keyed on ZOOM_CLIENT_ID.
//
// The From identity (EMAIL_FROM_ADDRESS, EMAIL_FROM_NAME) genuinely does stand alone, and
// resolves field by field.
type EnvSettings struct {
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPass     string
	SMTPTLS      bool
	SMTPStartTLS bool
	ResendAPIKey string

	EmailFrom     string
	EmailFromName string

	GoogleClientID     string
	GoogleClientSecret string

	ZoomClientID     string
	ZoomClientSecret string
}

// Built-in fallbacks, applied only when neither the environment nor server_settings
// supplies a value. They live here rather than in config.Load so that an unset variable
// stays distinguishable from a set one; see EnvSettings.
const (
	defaultSMTPPort      = "587"
	defaultEmailFrom     = "bookings@localhost"
	defaultEmailFromName = "Calnode"
)

// EmailTransportFromEnv reports whether the environment names an email transport, and so
// owns every transport credential. Both the merge and the admin UI need the answer.
func (e EnvSettings) EmailTransportFromEnv() bool {
	return e.SMTPHost != "" || e.ResendAPIKey != ""
}

// SetEnvSettings records the environment-supplied settings. Called once at boot from
// server.BuildHandler. The zero value means "nothing came from the environment", which is
// what every test constructing a Handler directly gets, and which leaves the stored row in
// sole charge exactly as before.
func (h *Handler) SetEnvSettings(env EnvSettings) { h.env = env }

// EnvSettings returns the environment-supplied settings, for handlers that report to the
// admin UI which fields it cannot usefully edit.
func (h *Handler) EnvSettings() EnvSettings { return h.env }

// MergeEmailSettings resolves the effective email configuration: the environment over the
// stored row, per the rules on EnvSettings. stored may be nil (nothing configured, or
// unreadable). Returns nil when the result names no transport at all, which is the same
// "email is not configured" signal LoadEmailSettingsFromDB has always returned.
func MergeEmailSettings(env EnvSettings, stored *SMTPConfig) *SMTPConfig {
	var out SMTPConfig

	// The transport credentials move as one unit; see EnvSettings for why.
	switch {
	case env.EmailTransportFromEnv():
		out.Host = env.SMTPHost
		out.Port = env.SMTPPort
		out.User = env.SMTPUser
		out.Pass = env.SMTPPass
		out.TLS = env.SMTPTLS
		out.StartTLS = env.SMTPStartTLS
		out.ResendAPIKey = env.ResendAPIKey
	case stored != nil:
		out.Host = stored.Host
		out.Port = stored.Port
		out.User = stored.User
		out.Pass = stored.Pass
		out.TLS = stored.TLS
		out.StartTLS = stored.StartTLS
		out.ResendAPIKey = stored.ResendAPIKey
	}

	// The From identity stands alone, so it resolves field by field.
	out.From = env.EmailFrom
	out.FromName = env.EmailFromName
	if stored != nil {
		if out.From == "" {
			out.From = stored.From
		}
		if out.FromName == "" {
			out.FromName = stored.FromName
		}
	}

	if out.Port == "" {
		out.Port = defaultSMTPPort
	}
	if out.From == "" {
		out.From = defaultEmailFrom
	}
	if out.FromName == "" {
		out.FromName = defaultEmailFromName
	}

	if out.Host == "" && out.ResendAPIKey == "" {
		return nil
	}
	return &out
}

// MergeGoogleSettings resolves the effective Google OAuth app. The client ID and secret
// move together: GOOGLE_CLIENT_ID set means the environment supplies both, so a stored
// secret is never paired with an environment ID.
func MergeGoogleSettings(env EnvSettings, stored *GoogleOAuthConfig) *GoogleOAuthConfig {
	if env.GoogleClientID != "" {
		return &GoogleOAuthConfig{
			ClientID:     env.GoogleClientID,
			ClientSecret: env.GoogleClientSecret,
		}
	}
	return stored
}

// MergeZoomSettings resolves the effective Zoom OAuth app, on the same pairing rule as
// MergeGoogleSettings.
func MergeZoomSettings(env EnvSettings, stored *ZoomOAuthConfig) *ZoomOAuthConfig {
	if env.ZoomClientID != "" {
		return &ZoomOAuthConfig{
			ClientID:     env.ZoomClientID,
			ClientSecret: env.ZoomClientSecret,
		}
	}
	return stored
}

// resolveEmailSettings returns the effective email configuration, environment over stored.
//
// A failed read of the stored row is logged and treated as "nothing stored" rather than
// propagated. The reason is the point of the whole change: an environment configuration
// that works must not be masked by a database value that cannot be read -- a row encrypted
// under a key this process no longer holds is exactly the situation in which an operator
// reaches for an environment variable.
func (h *Handler) resolveEmailSettings(ctx context.Context) *SMTPConfig {
	stored, err := LoadEmailSettingsFromDB(h.db, h.encKey)
	if err != nil {
		h.logger.ErrorContext(ctx, "email settings: read stored settings", "error", err)
	}
	return MergeEmailSettings(h.env, stored)
}
