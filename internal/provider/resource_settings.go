package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/disc/terraform-provider-pritunl/internal/pritunl"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

// A Pritunl instance exposes a single, instance wide settings object, so the
// resource is a singleton and uses a fixed identifier instead of an API
// provided one. It is also the import id of the resource.
const settingsResourceId = "settings"

const (
	// Pritunl schedules the web server restart shortly after answering the
	// request, so the endpoint is given a head start before being polled and
	// has to answer a few times in a row to be considered back
	settingsRestartDelay     = 5 * time.Second
	settingsRestartPoll      = 2 * time.Second
	settingsRestartSettle    = time.Second
	settingsRestartSuccesses = 3
	settingsRestartTimeout   = 2 * time.Minute
)

func resourceSettings() *schema.Resource {
	return &schema.Resource{
		Description: "The settings resource allows managing the global settings of a Pritunl instance, in particular the TLS certificate served by its own web console and API. It is a singleton resource: a single instance of it maps to the whole Pritunl instance and its import id is always `settings`.",
		Schema: map[string]*schema.Schema{
			// the certificate fields are computed so that leaving them out of
			// the configuration keeps the certificate of the instance as it is
			// instead of asking to remove it, it is only handed back to Pritunl
			// when the resource itself is destroyed
			"server_cert": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				RequiredWith: []string{"server_key"},
				StateFunc:    trimSpaceStateFunc,
				Description:  "The PEM encoded certificate served by the web console and the API. It must contain the full chain, the leaf certificate followed by the intermediate certificate(s) concatenated, otherwise Pritunl will not validate and accept it correctly. Destroying the resource hands the certificate back to Pritunl, which regenerates its self-signed default.",
			},
			"server_key": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				Sensitive:    true,
				RequiredWith: []string{"server_cert"},
				StateFunc:    trimSpaceStateFunc,
				Description:  "The PEM encoded private key matching `server_cert`. It is write-only: the value is never read back from the Pritunl API, so it is neither refreshed nor populated on import.",
			},
			"server_port": {
				Type:         schema.TypeInt,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.IntBetween(1, 65535),
				Description:  "The port the web console and the API listen on. Defaults to the port already configured on the instance. Changing it also requires updating the `url` of the provider.",
			},
		},
		CreateContext: resourceCreateSettings,
		ReadContext:   resourceReadSettings,
		UpdateContext: resourceUpdateSettings,
		DeleteContext: resourceDeleteSettings,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
	}
}

// Pritunl strips the surrounding whitespace of the PEM values before storing
// them, so the configured value is normalised the same way to keep it in sync
// with what the API returns.
func trimSpaceStateFunc(value interface{}) string {
	return strings.TrimSpace(value.(string))
}

func resourceCreateSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	// the settings object always exists, there is nothing to create: only the
	// managed fields are pushed and every other setting is left untouched
	current, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	settings := settingsPayload(d, current)

	portChanged := false
	if v, ok := d.GetOk("server_port"); ok {
		settings.ServerPort = v.(int)
		portChanged = settings.ServerPort != current.ServerPort
	}

	if err = apiClient.UpdateSettings(settings); err != nil {
		return diag.FromErr(err)
	}

	d.SetId(settingsResourceId)

	return finishSettingsWrite(ctx, d, meta, portChanged)
}

// Uses for importing
func resourceReadSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	settings, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	d.Set("server_cert", derefString(settings.ServerCert))
	d.Set("server_port", settings.ServerPort)

	// GET /settings does return server_key in plain text, but it is deliberately
	// not stored: it would copy the private key of the instance into the state
	// of every configuration that only manages server_port, and its absence
	// from the state is what tells the delete that this resource never pushed a
	// certificate of its own.

	return nil
}

func resourceUpdateSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	portChanged := d.HasChange("server_port")

	if !portChanged && !d.HasChange("server_cert") && !d.HasChange("server_key") {
		return resourceReadSettings(ctx, d, meta)
	}

	current, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	settings := settingsPayload(d, current)

	if v, ok := d.GetOk("server_port"); ok {
		settings.ServerPort = v.(int)
	}

	if err = apiClient.UpdateSettings(settings); err != nil {
		return diag.FromErr(err)
	}

	return finishSettingsWrite(ctx, d, meta, portChanged)
}

// Pritunl regenerates a self-signed certificate whenever it restarts its web
// server for a settings write that did not carry server_cert and server_key, so
// the pair is part of every payload, even when only the port changes. The
// configured pair is used when this resource manages one, otherwise the pair
// already installed on the instance is sent back untouched. Both always travel
// together: a key without its certificate would leave Pritunl unable to serve
// HTTPS at all.
func settingsPayload(d *schema.ResourceData, current *pritunl.Settings) *pritunl.Settings {
	settings := &pritunl.Settings{}

	cert := strings.TrimSpace(d.Get("server_cert").(string))
	key := strings.TrimSpace(d.Get("server_key").(string))

	if cert == "" || key == "" {
		cert = strings.TrimSpace(derefString(current.ServerCert))
		key = strings.TrimSpace(derefString(current.ServerKey))
	}

	if cert != "" && key != "" {
		settings.ServerCert = &cert
		settings.ServerKey = &key
	}

	return settings
}

func resourceDeleteSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	// server_key is never refreshed from the api, so it only holds a value when
	// this resource pushed a certificate itself. Without it the certificate in
	// the state merely comes from a read and must not be touched.
	if d.Get("server_key").(string) == "" {
		d.SetId("")

		return nil
	}

	// the settings object cannot be deleted, destroying the resource resets the
	// managed certificate instead. An empty string is enough to clear it: the
	// API turns any falsy value into null, and Pritunl then regenerates its
	// self-signed default while restarting the web server. server_port is left
	// alone, the instance still has to be reachable on the same port.
	empty := ""
	settings := &pritunl.Settings{
		ServerCert: &empty,
		ServerKey:  &empty,
	}

	if err := apiClient.UpdateSettings(settings); err != nil {
		return diag.FromErr(err)
	}

	if err := waitForWebServer(ctx, apiClient); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")

	return nil
}

func finishSettingsWrite(ctx context.Context, d *schema.ResourceData, meta interface{}, portChanged bool) diag.Diagnostics {
	if portChanged {
		// the web console and the API are served on another port from now on,
		// waiting for them on the previous one or reading the settings back
		// would only time out
		return diag.Diagnostics{{
			Severity: diag.Warning,
			Summary:  "The Pritunl web server port has been changed",
			Detail:   "Pritunl restarted its web server on the new port, the `url` of the pritunl provider has to be updated accordingly before the next Terraform run.",
		}}
	}

	if err := waitForWebServer(ctx, meta.(pritunl.Client)); err != nil {
		return diag.FromErr(err)
	}

	return resourceReadSettings(ctx, d, meta)
}

// Applying a certificate, a key or a port makes Pritunl restart its web server
// shortly after it answered the request, which leaves the API unreachable for a
// moment. Reading the settings back right away would fail, and the previous
// server is still up for a short while, so the endpoint has to answer a few
// times in a row before it is considered restarted.
func waitForWebServer(ctx context.Context, apiClient pritunl.Client) error {
	if err := sleepContext(ctx, settingsRestartDelay); err != nil {
		return err
	}

	deadline := time.Now().Add(settingsRestartTimeout)
	successes := 0

	for {
		err := apiClient.TestApiCall()
		if err == nil {
			successes++
			if successes >= settingsRestartSuccesses {
				return nil
			}

			if err = sleepContext(ctx, settingsRestartSettle); err != nil {
				return err
			}

			continue
		}

		successes = 0

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout while waiting for the pritunl web server to restart with the new settings: %s", err)
		}

		if err = sleepContext(ctx, settingsRestartPoll); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
