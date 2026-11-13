package pritunl

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeSettings(t *testing.T, body string) Settings {
	t.Helper()

	settings := Settings{}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil {
		t.Fatalf("failed to decode the settings: %s", err)
	}

	return settings
}

func TestSettingsAuditing(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"all is on":             {`{"auditing": "all"}`, true},
		"null is off":           {`{"auditing": null}`, false},
		"absent is off":         {`{}`, false},
		"another string is off": {`{"auditing": "none"}`, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decodeSettings(t, c.body).Auditing(); got != c.want {
				t.Fatalf("Auditing() of %s: got %t, want %t", c.body, got, c.want)
			}
		})
	}
}

func TestSettingsSetAuditing(t *testing.T) {
	cases := map[string]struct {
		on   bool
		want string
	}{
		"on is all":   {true, `{"auditing":"all"}`},
		"off is null": {false, `{"auditing":null}`},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			settings := decodeSettings(t, `{"auditing": "all"}`)
			if c.on {
				settings = decodeSettings(t, `{"auditing": null}`)
			}

			settings.SetAuditing(c.on)

			body, err := json.Marshal(settings)
			if err != nil {
				t.Fatalf("failed to encode the settings: %s", err)
			}
			if string(body) != c.want {
				t.Fatalf("SetAuditing(%t): got %s, want %s", c.on, body, c.want)
			}
			if settings.Auditing() != c.on {
				t.Fatalf("SetAuditing(%t) does not read back as %t", c.on, c.on)
			}
		})
	}
}

// A number reads the same whether the API returned it or this provider wrote it.
func TestSettingsInt(t *testing.T) {
	settings := decodeSettings(t, `{"server_port": 8443}`)
	if got := settings.Int("server_port"); got != 8443 {
		t.Fatalf("a number read from the API: got %d, want 8443", got)
	}

	settings["server_port"] = 443
	if got := settings.Int("server_port"); got != 443 {
		t.Fatalf("a number written by the provider: got %d, want 443", got)
	}

	if got := settings.Int("absent"); got != 0 {
		t.Fatalf("an absent setting: got %d, want 0", got)
	}
}
