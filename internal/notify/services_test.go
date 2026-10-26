package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var sampleConfigs = map[string]map[string]string{
	"smtp":     {"host": "smtp.example.com", "port": "587", "username": "u@example.com", "password": "p@ss/w:rd#1", "encryption": "starttls", "from": "docksmith@example.com", "to": "a@example.com,b@example.com"},
	"ntfy":     {"host": "ntfy.example.com", "topic": "docksmith", "token": "tk_abc123", "username": "bob", "password": "p@ss w/rd"},
	"discord":  {"webhook_url": "https://discord.com/api/webhooks/123456789/AbCdEf-gh_ij"},
	"telegram": {"token": "123456:ABC-DEF1234ghIkl", "chats": "@chan1,12345"},
	"pushover": {"token": "azGDORePK8gMaC0QOYAMyEEuzJnyUi", "user_key": "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"},
	"gotify":   {"host": "gotify.example.com:8080", "token": "AbCdEfGhIjKlMnO"},
	"slack":    {"webhook_url": "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"},
	"webhook":  {"url": "https://example.com/hook/secretpath?k=v"},
	"advanced": {"url": "ntfy://ntfy.sh/mytopic"},
}

func TestBuildURL_AllTypesParse(t *testing.T) {
	for _, typ := range ServiceTypes {
		t.Run(typ, func(t *testing.T) {
			cfg, ok := sampleConfigs[typ]
			require.True(t, ok)
			u, err := BuildURL(typ, cfg)
			require.NoError(t, err)
			_, err = shoutrrr.CreateSender(u)
			require.NoError(t, err, u)
		})
	}
}

func TestBuildURL_SMTPEscapesPassword(t *testing.T) {
	u, err := BuildURL("smtp", sampleConfigs["smtp"])
	require.NoError(t, err)
	assert.NotContains(t, u, "p@ss/w:rd#1")
	assert.Contains(t, u, "usehtml=yes")
}

func TestMergeSecrets_KeepsStoredWhenMasked(t *testing.T) {
	got := MergeSecrets("smtp",
		map[string]string{"password": MaskedSecret, "host": "new"},
		map[string]string{"password": "hunter2", "host": "old"})
	assert.Equal(t, map[string]string{"password": "hunter2", "host": "new"}, got)
}

func TestMaskConfig_HidesSecrets(t *testing.T) {
	in := map[string]string{"token": "t", "password": "p", "host": "h", "username": ""}
	got := MaskConfig("ntfy", in)
	assert.Equal(t, MaskedSecret, got["token"])
	assert.Equal(t, MaskedSecret, got["password"])
	assert.Equal(t, "h", got["host"])
	assert.Equal(t, "t", in["token"], "input must not be mutated")
}

func TestValidateServiceConfig_SMTPRequiresHostFromTo(t *testing.T) {
	assert.Error(t, ValidateServiceConfig("smtp", map[string]string{"from": "a@b.c", "to": "d@e.f"}))
	assert.Error(t, ValidateServiceConfig("smtp", map[string]string{"host": "h", "to": "d@e.f"}))
	assert.Error(t, ValidateServiceConfig("smtp", map[string]string{"host": "h", "from": "a@b.c"}))
	assert.NoError(t, ValidateServiceConfig("smtp", sampleConfigs["smtp"]))
	assert.Error(t, ValidateServiceConfig("nope", nil))
}

func TestShoutrrrSender_ErrorsRedactSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	secretPath := "SuperSecretToken123"
	cfg := map[string]string{"url": srv.URL + "/" + secretPath}

	err := ShoutrrrSender{}.Send(context.Background(), "webhook", cfg, Message{Title: "t", Text: "hi"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretPath)
	assert.NotContains(t, err.Error(), cfg["url"])

	// Failing before the network (bad URL) must not leak either.
	err = ShoutrrrSender{}.Send(context.Background(), "advanced", map[string]string{"url": "bogus://user:pa55w0rdXYZ@host/tok3nXYZ"}, Message{Text: "x"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "pa55w0rdXYZ")
	assert.NotContains(t, err.Error(), "tok3nXYZ")
}

func TestShoutrrrSender_RespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := ShoutrrrSender{}.Send(ctx, "webhook", map[string]string{"url": srv.URL + "/h"}, Message{Text: "hi"})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.False(t, strings.Contains(err.Error(), srv.URL+"/h"))
}

func TestRedactError_StripsSecretsAndURL(t *testing.T) {
	for typ, cfg := range sampleConfigs {
		u, err := BuildURL(typ, cfg)
		require.NoError(t, err)
		raw := errors.New("failed: Post \"" + u + "\": dial tcp: refused")
		got := redactError(raw, typ, cfg, u).Error()
		assert.NotContains(t, got, u, typ)
		for _, k := range SecretFields(typ) {
			assert.NotContains(t, got, cfg[k], typ+"/"+k)
		}
	}
}
