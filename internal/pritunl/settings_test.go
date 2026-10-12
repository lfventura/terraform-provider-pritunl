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
