package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// settingsDeleteClient answers the calls the destroy of pritunl_settings
// makes; any other call panics on the nil embedded client.
type settingsDeleteClient struct {
	pritunl.Client
	updateErr error
	pingErr   error
	written   pritunl.Settings
}

func (c *settingsDeleteClient) GetSettings() (pritunl.Settings, error) {
	return pritunl.Settings{"server_cert": "cert", "server_key": "key", "server_port": 443}, nil
}

func (c *settingsDeleteClient) UpdateSettings(settings pritunl.Settings) error {
	c.written = settings

	return c.updateErr
}

func (c *settingsDeleteClient) PingWebServer(ctx context.Context) error {
	return c.pingErr
}

func managedCertificateSettings(t *testing.T) *schema.ResourceData {
	t.Helper()

	d := schema.TestResourceDataRaw(t, resourceSettings().Schema, map[string]interface{}{
		"server_cert": "cert",
		"server_key":  "key",
	})
	d.SetId("settings")

	return d
}

func TestDeleteSettingsWebServerSilentIsAWarning(t *testing.T) {
	client := &settingsDeleteClient{pingErr: errors.New("context deadline exceeded")}
	d := managedCertificateSettings(t)

	// the wait gives up as soon as its context does
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	diags := resourceDeleteSettings(ctx, d, client)

	if diags.HasError() {
		t.Fatalf("expected no error, got %v", diags)
	}
	if len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Fatalf("expected one warning, got %v", diags)
	}
	if d.Id() != "" {
		t.Fatalf("expected the resource to leave the state, id is %q", d.Id())
	}
	if client.written["server_cert"] != "" || client.written["server_key"] != "" {
		t.Fatalf("expected the certificate to be reset, wrote %v", client.written)
	}
	if client.written["server_port"] != 443 {
		t.Fatalf("expected the other settings to be handed back untouched, wrote %v", client.written)
	}
}

func TestDeleteSettingsRejectedResetIsAnError(t *testing.T) {
	client := &settingsDeleteClient{updateErr: errors.New("400 invalid")}
	d := managedCertificateSettings(t)

	diags := resourceDeleteSettings(context.Background(), d, client)

	if !diags.HasError() {
		t.Fatalf("expected an error, got %v", diags)
	}
	if d.Id() != "settings" {
		t.Fatalf("expected the resource to stay in the state, id is %q", d.Id())
	}
}
