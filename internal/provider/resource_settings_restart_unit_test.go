package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// restartingInstance is a Pritunl instance whose web server restarts when a
// write changes the certificate, its key or the port, the way the real one
// does: it answers no probe for a number of them, then answers again.
type restartingInstance struct {
	pritunl.Client

	mu       sync.Mutex
	settings pritunl.Settings

	// downProbes is how many probes fail after a restart
	downProbes int
	remaining  int

	writes int
	probes int
	// answered counts the probes answered since the last restart
	answered int
}

func newRestartingInstance(downProbes int) *restartingInstance {
	return &restartingInstance{
		settings: pritunl.Settings{
			"server_cert": "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----",
			"server_key":  "-----BEGIN PRIVATE KEY-----\nold\n-----END PRIVATE KEY-----",
			"server_port": json.Number("443"),
			"pin_mode":    "optional",
		},
		downProbes: downProbes,
	}
}

func (i *restartingInstance) GetSettings() (pritunl.Settings, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	copied := pritunl.Settings{}
	for key, value := range i.settings {
		copied[key] = value
	}

	return copied, nil
}

func (i *restartingInstance) UpdateSettings(settings pritunl.Settings) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.writes++
	if webServerSettingsOf(settings) != webServerSettingsOf(i.settings) {
		i.remaining = i.downProbes
		i.answered = 0
	}
	i.settings = settings

	return nil
}

func (i *restartingInstance) PingWebServer(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.probes++
	if i.remaining > 0 {
		i.remaining--

		return errors.New("connection refused")
	}
	i.answered++

	return nil
}

// quickRestartWait shortens the wait for the length of a test.
func quickRestartWait(t *testing.T, timeout time.Duration) {
	t.Helper()

	delay, poll, settle, limit := settingsRestartDelay, settingsRestartPoll, settingsRestartSettle, settingsRestartTimeout
	settingsRestartDelay, settingsRestartPoll, settingsRestartSettle, settingsRestartTimeout = 0, time.Millisecond, time.Millisecond, timeout
	t.Cleanup(func() {
		settingsRestartDelay, settingsRestartPoll, settingsRestartSettle, settingsRestartTimeout = delay, poll, settle, limit
	})
}

func certificateConfig(cert, key string) (map[string]interface{}, map[string]cty.Value) {
	return map[string]interface{}{"server_cert": cert, "server_key": key},
		map[string]cty.Value{"server_cert": cty.StringVal(cert), "server_key": cty.StringVal(key)}
}

func hasError(diags diag.Diagnostics) bool {
	for _, d := range diags {
		if d.Severity == diag.Error {
			return true
		}
	}

	return false
}

func TestSettingsWaitsForTheWebServerAfterACertificateChange(t *testing.T) {
	quickRestartWait(t, time.Minute)

	for name, write := range map[string]func(context.Context, *restartingInstance, map[string]interface{}, map[string]cty.Value) diag.Diagnostics{
		"create": func(ctx context.Context, i *restartingInstance, raw map[string]interface{}, config map[string]cty.Value) diag.Diagnostics {
			return resourceCreateSettings(ctx, settingsResourceData(t, raw, config), i)
		},
		"update": func(ctx context.Context, i *restartingInstance, raw map[string]interface{}, config map[string]cty.Value) diag.Diagnostics {
			return resourceUpdateSettings(ctx, settingsResourceData(t, raw, config), i)
		},
	} {
		t.Run(name, func(t *testing.T) {
			instance := newRestartingInstance(7)
			raw, config := certificateConfig("-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----", "-----BEGIN PRIVATE KEY-----\nnew\n-----END PRIVATE KEY-----")

			if diags := write(context.Background(), instance, raw, config); hasError(diags) {
				t.Fatalf("the write failed: %v", diags)
			}

			if instance.probes < 7+settingsRestartSuccesses {
				t.Fatalf("the write came back after %d probes, before the web server answered again", instance.probes)
			}
			if instance.answered < settingsRestartSuccesses {
				t.Fatalf("the write came back after %d answered probes, %d in a row are due", instance.answered, settingsRestartSuccesses)
			}
		})
	}
}

func TestSettingsWaitsAfterAKeyChangeAlone(t *testing.T) {
	quickRestartWait(t, time.Minute)

	instance := newRestartingInstance(4)
	raw, config := certificateConfig(instance.settings.String("server_cert"), "-----BEGIN PRIVATE KEY-----\nnew\n-----END PRIVATE KEY-----")

	if diags := resourceCreateSettings(context.Background(), settingsResourceData(t, raw, config), instance); hasError(diags) {
		t.Fatalf("the write failed: %v", diags)
	}

	if instance.probes < 4+settingsRestartSuccesses {
		t.Fatalf("a changed key restarts the web server, the write came back after %d probes", instance.probes)
	}
}

// Nothing restarts the web server, so there is nothing to poll for.
func TestSettingsDoesNotPollWhenNothingRestartsTheWebServer(t *testing.T) {
	quickRestartWait(t, time.Minute)

	cert, key := "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----", "-----BEGIN PRIVATE KEY-----\nold\n-----END PRIVATE KEY-----"

	cases := map[string]struct {
		raw    map[string]interface{}
		config map[string]cty.Value
	}{
		"the same certificate and key": func() struct {
			raw    map[string]interface{}
			config map[string]cty.Value
		} {
			raw, config := certificateConfig(cert, key)
			raw["pin_mode"], config["pin_mode"] = "required", cty.StringVal("required")

			return struct {
				raw    map[string]interface{}
				config map[string]cty.Value
			}{raw, config}
		}(),
		"the same certificate with surrounding whitespace": func() struct {
			raw    map[string]interface{}
			config map[string]cty.Value
		} {
			raw, config := certificateConfig("\n"+cert+"\n", key+"\n")

			return struct {
				raw    map[string]interface{}
				config map[string]cty.Value
			}{raw, config}
		}(),
		"a toggle alone": {
			raw:    map[string]interface{}{"pin_mode": "required"},
			config: map[string]cty.Value{"pin_mode": cty.StringVal("required")},
		},
	}

	for name, c := range cases {
		for _, op := range []string{"create", "update"} {
			t.Run(name+" "+op, func(t *testing.T) {
				instance := newRestartingInstance(5)
				d := settingsResourceData(t, c.raw, c.config)

				var diags diag.Diagnostics
				if op == "create" {
					diags = resourceCreateSettings(context.Background(), d, instance)
				} else {
					diags = resourceUpdateSettings(context.Background(), d, instance)
				}

				if hasError(diags) {
					t.Fatalf("the write failed: %v", diags)
				}
				if instance.writes != 1 {
					t.Fatalf("expected the write to reach the instance, got %d writes", instance.writes)
				}
				if instance.probes != 0 {
					t.Fatalf("no restart is due, yet the web server was probed %d times", instance.probes)
				}
			})
		}
	}
}

// The port is the one change the provider cannot wait out: the console is
// served elsewhere from then on, and its url is a provider argument.
func TestSettingsDoesNotPollTheOldPortAfterAPortChange(t *testing.T) {
	quickRestartWait(t, time.Minute)

	instance := newRestartingInstance(3)
	raw, config := map[string]interface{}{"server_port": 8443}, map[string]cty.Value{"server_port": cty.NumberIntVal(8443)}

	diags := resourceCreateSettings(context.Background(), settingsResourceData(t, raw, config), instance)

	if hasError(diags) || len(diags) != 1 || !strings.Contains(diags[0].Summary, "port has been changed") {
		t.Fatalf("expected the port warning only, got %v", diags)
	}
	if instance.probes != 0 {
		t.Fatalf("the old port must not be probed, got %d probes", instance.probes)
	}
}

func TestSettingsFailsWhenTheWebServerDoesNotComeBack(t *testing.T) {
	quickRestartWait(t, 50*time.Millisecond)

	instance := newRestartingInstance(1 << 30)
	raw, config := certificateConfig("-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----", "-----BEGIN PRIVATE KEY-----\nnew\n-----END PRIVATE KEY-----")

	diags := resourceCreateSettings(context.Background(), settingsResourceData(t, raw, config), instance)

	if !hasError(diags) || !strings.Contains(diags[0].Summary, "timeout while waiting for the pritunl web server") {
		t.Fatalf("expected the timeout, got %v", diags)
	}
}

func TestSettingsWaitHonoursTheContextDeadline(t *testing.T) {
	quickRestartWait(t, time.Hour)

	instance := newRestartingInstance(1 << 30)
	raw, config := certificateConfig("-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----", "-----BEGIN PRIVATE KEY-----\nnew\n-----END PRIVATE KEY-----")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	diags := resourceCreateSettings(ctx, settingsResourceData(t, raw, config), instance)

	if !hasError(diags) || !strings.Contains(diags[0].Summary, "context deadline exceeded") {
		t.Fatalf("expected the context deadline, got %v", diags)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait outlived the deadline of its context: %s", elapsed)
	}
}
