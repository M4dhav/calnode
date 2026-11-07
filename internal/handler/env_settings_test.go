package handler

import "testing"

// The contract these pin: a setting present in the environment is the one that takes
// effect, and server_settings is the fallback for a setting the environment omits.
//
// The old precedence was the reverse, and had no test at all — which is how it survived
// long enough to make every redeployed change to EMAIL_SMTP_HOST a no-op.

func TestMergeEmailSettings_environmentOutranksStored(t *testing.T) {
	stored := &SMTPConfig{
		Host: "smtp.stored.example", Port: "465", User: "stored-user", Pass: "stored-pass",
		TLS: true, From: "stored@example.com", FromName: "Stored",
	}
	env := EnvSettings{
		SMTPHost: "smtp.env.example", SMTPPort: "587", SMTPUser: "env-user",
		SMTPPass: "env-pass", SMTPStartTLS: true,
	}

	got := MergeEmailSettings(env, stored)
	if got == nil {
		t.Fatal("MergeEmailSettings returned nil; want the environment configuration")
	}
	if got.Host != "smtp.env.example" || got.Port != "587" {
		t.Errorf("host/port = %q/%q; want smtp.env.example/587", got.Host, got.Port)
	}
	// The whole connection moves together. Carrying the stored password over to the
	// environment's host would be a credential half from each source: it authenticates
	// against nothing and fails at the SMTP handshake, not at boot.
	if got.User != "env-user" || got.Pass != "env-pass" {
		t.Errorf("user/pass = %q/%q; want env-user/env-pass (stored values must not leak across)", got.User, got.Pass)
	}
	if got.TLS || !got.StartTLS {
		t.Errorf("tls/starttls = %v/%v; want false/true from the environment", got.TLS, got.StartTLS)
	}
	// The From identity stands alone, so an unset EMAIL_FROM_ADDRESS still falls back.
	if got.From != "stored@example.com" || got.FromName != "Stored" {
		t.Errorf("from = %q <%q>; want the stored identity, which the environment did not set", got.FromName, got.From)
	}
}

func TestMergeEmailSettings_storedUsedWhenEnvironmentIsSilent(t *testing.T) {
	stored := &SMTPConfig{
		Host: "smtp.stored.example", Port: "465", User: "u", Pass: "p",
		TLS: true, From: "stored@example.com", FromName: "Stored",
	}
	got := MergeEmailSettings(EnvSettings{}, stored)
	if got == nil {
		t.Fatal("MergeEmailSettings returned nil; want the stored configuration")
	}
	if *got != *stored {
		t.Errorf("merged = %+v; want the stored config unchanged %+v", *got, *stored)
	}
}

// The trap that makes EMAIL_RESEND_API_KEY necessary. BuildMailer picks Resend whenever a
// key is present, so merging the stored key alongside an environment SMTP host would route
// mail over HTTPS and leave EMAIL_SMTP_HOST with no effect — the exact bug being fixed,
// reintroduced one layer down.
func TestMergeEmailSettings_environmentChoosesTheTransport(t *testing.T) {
	t.Run("env SMTP host drops a stored Resend key", func(t *testing.T) {
		stored := &SMTPConfig{ResendAPIKey: "re_stored", From: "stored@example.com"}
		got := MergeEmailSettings(EnvSettings{SMTPHost: "smtp.env.example"}, stored)
		if got == nil {
			t.Fatal("merged to nil; want the environment SMTP host")
		}
		if got.ResendAPIKey != "" {
			t.Errorf("resend key = %q; want it dropped so the environment's SMTP host decides", got.ResendAPIKey)
		}
		if _, transport := BuildMailer(*got); transport != TransportSMTP {
			t.Errorf("transport = %q; want %q", transport, TransportSMTP)
		}
	})

	t.Run("env Resend key drops a stored SMTP host", func(t *testing.T) {
		stored := &SMTPConfig{Host: "smtp.stored.example", Port: "465"}
		got := MergeEmailSettings(EnvSettings{ResendAPIKey: "re_env"}, stored)
		if got == nil {
			t.Fatal("merged to nil; want the environment Resend key")
		}
		if got.Host != "" {
			t.Errorf("host = %q; want it dropped so the environment's API key decides", got.Host)
		}
		if _, transport := BuildMailer(*got); transport != TransportResend {
			t.Errorf("transport = %q; want %q", transport, TransportResend)
		}
	})
}

// config.Load must not bake defaults into fields server_settings also holds. If it did,
// EMAIL_FROM_ADDRESS would arrive as "bookings@localhost" on an instance where nobody set
// it, and would then outrank — and overwrite the effect of — every stored From address.
func TestMergeEmailSettings_unsetFieldsFallBackThenDefault(t *testing.T) {
	stored := &SMTPConfig{Host: "smtp.stored.example", Port: "2525", From: "stored@example.com", FromName: "Stored"}
	if got := MergeEmailSettings(EnvSettings{}, stored); got.From != "stored@example.com" || got.Port != "2525" {
		t.Errorf("from/port = %q/%q; want the stored values to survive an empty environment", got.From, got.Port)
	}

	// Built-in defaults apply only when neither source supplies a value.
	got := MergeEmailSettings(EnvSettings{SMTPHost: "smtp.env.example"}, nil)
	if got == nil {
		t.Fatal("merged to nil; want the environment host")
	}
	if got.Port != defaultSMTPPort || got.From != defaultEmailFrom || got.FromName != defaultEmailFromName {
		t.Errorf("defaults = %q/%q/%q; want %q/%q/%q",
			got.Port, got.From, got.FromName, defaultSMTPPort, defaultEmailFrom, defaultEmailFromName)
	}
}

func TestMergeEmailSettings_nilWhenNothingNamesATransport(t *testing.T) {
	if got := MergeEmailSettings(EnvSettings{}, nil); got != nil {
		t.Errorf("merged = %+v; want nil (email unconfigured)", *got)
	}
	// A From address alone is not a transport, so it must not read as configured.
	if got := MergeEmailSettings(EnvSettings{EmailFrom: "a@example.com"}, nil); got != nil {
		t.Errorf("merged = %+v; want nil — a From address is not a delivery path", *got)
	}
}

func TestMergeOAuthApps_idAndSecretMoveTogether(t *testing.T) {
	t.Run("google", func(t *testing.T) {
		stored := &GoogleOAuthConfig{ClientID: "stored-id", ClientSecret: "stored-secret"}

		got := MergeGoogleSettings(EnvSettings{GoogleClientID: "env-id", GoogleClientSecret: "env-secret"}, stored)
		if got.ClientID != "env-id" || got.ClientSecret != "env-secret" {
			t.Errorf("app = %q/%q; want env-id/env-secret", got.ClientID, got.ClientSecret)
		}

		// GOOGLE_CLIENT_ID without its secret must NOT borrow the stored one: a mismatched
		// pair is refused at Google's consent screen, far from anything that explains it.
		got = MergeGoogleSettings(EnvSettings{GoogleClientID: "env-id"}, stored)
		if got.ClientSecret != "" {
			t.Errorf("secret = %q; want empty rather than the stored secret of a different app", got.ClientSecret)
		}

		if got = MergeGoogleSettings(EnvSettings{}, stored); got != stored {
			t.Errorf("app = %+v; want the stored app when the environment names none", got)
		}
		if got = MergeGoogleSettings(EnvSettings{}, nil); got != nil {
			t.Errorf("app = %+v; want nil when neither source configures one", got)
		}
	})

	t.Run("zoom", func(t *testing.T) {
		stored := &ZoomOAuthConfig{ClientID: "stored-id", ClientSecret: "stored-secret"}

		got := MergeZoomSettings(EnvSettings{ZoomClientID: "env-id", ZoomClientSecret: "env-secret"}, stored)
		if got.ClientID != "env-id" || got.ClientSecret != "env-secret" {
			t.Errorf("app = %q/%q; want env-id/env-secret", got.ClientID, got.ClientSecret)
		}
		got = MergeZoomSettings(EnvSettings{ZoomClientID: "env-id"}, stored)
		if got.ClientSecret != "" {
			t.Errorf("secret = %q; want empty rather than the stored secret of a different app", got.ClientSecret)
		}
		if got = MergeZoomSettings(EnvSettings{}, stored); got != stored {
			t.Errorf("app = %+v; want the stored app when the environment names none", got)
		}
	})
}
