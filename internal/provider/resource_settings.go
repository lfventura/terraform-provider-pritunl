package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
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
		Description: "The settings resource allows managing the global settings of a Pritunl instance, in particular the TLS certificate served by its own web console and API. It is a singleton resource: a single instance of it maps to the whole Pritunl instance and its import id is always `settings`. Every write is a read-modify-write of the complete settings object, the same way the Pritunl web console itself works: `PUT /settings` is a full replace and not a partial update, so the settings are read back from the instance immediately before each write and handed over again with the managed attributes overlaid on top of them. That is what keeps unmanaged settings such as the single sign-on, SMTP or monitoring configuration untouched, and only the managed attributes are ever stored in the Terraform state.",
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
				Description:  "The certificate served by the web console and the API, as pure concatenated PEM blocks: the leaf certificate followed by the intermediate certificate(s), in that order. The root CA certificate must be left out, clients already trust it through their own trust store and only need the intermediates to build the chain. Nothing but `-----BEGIN CERTIFICATE-----` blocks may be present: the `Bag Attributes`, `subject=` and `issuer=` lines that `openssl pkcs12 -nokeys` writes in front of every block are OpenSSL specific text, not part of the PEM format, and Pritunl rejects a certificate that still carries them. Destroying the resource hands the certificate back to Pritunl, which regenerates its self-signed default.",
			},
			"server_key": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				Sensitive:    true,
				RequiredWith: []string{"server_cert"},
				StateFunc:    trimSpaceStateFunc,
				Description:  "The PEM encoded private key matching `server_cert`. It is write-only: the value is never read back from the Pritunl API, so it is neither refreshed nor populated on import. A side effect is that the plan can only compare the configured value against the previously configured one, never against what the instance is actually serving: if the key is changed on the instance outside of Terraform, the plan will not show it as changed on its own. It is still corrected whenever `server_cert` plans a change, since both are always written together; only a drift limited to `server_key` on its own goes undetected by the plan, and is applied on the next write that touches `server_cert`.",
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

	// the settings object always exists, there is nothing to create: the whole
	// object is read, the managed attributes are overlaid on it and all of it
	// is written back
	settings, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	portChanged := overlaySettings(d, settings)

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

	// only the managed attributes make it into the state, the rest of the
	// settings object is a write time detail and never leaves this package
	d.Set("server_cert", strings.TrimSpace(settings.String("server_cert")))
	d.Set("server_port", settings.Int("server_port"))

	// GET /settings does return server_key in plain text, but it is deliberately
	// not stored: it would copy the private key of the instance into the state
	// of every configuration that only manages server_port, and its absence
	// from the state is what tells the delete that this resource never pushed a
	// certificate of its own.

	return nil
}

func resourceUpdateSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	if !d.HasChange("server_port") && !d.HasChange("server_cert") && !d.HasChange("server_key") {
		return resourceReadSettings(ctx, d, meta)
	}

	settings, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	portChanged := overlaySettings(d, settings)

	if err = apiClient.UpdateSettings(settings); err != nil {
		return diag.FromErr(err)
	}

	return finishSettingsWrite(ctx, d, meta, portChanged)
}

// overlaySettings writes the attributes managed by this resource on top of the
// settings object that was just read from the instance, and reports whether the
// web server is about to move to another port.
//
// Everything it does not touch is left in the object exactly as the API
// returned it, which is what the surrounding full object round trip hands back.
// That includes the settings the API computes rather than stores, such as
// public_address, which falls back to the address Pritunl detected on its own:
// echoing back a value that was just read is a no-op, echoing back a stale one
// would pin it as a manual override, so the settings are always read as part of
// the write and never cached between runs.
func overlaySettings(d *schema.ResourceData, settings pritunl.Settings) bool {
	cert := strings.TrimSpace(d.Get("server_cert").(string))
	key := strings.TrimSpace(d.Get("server_key").(string))

	// the pair is only pushed when this resource manages one: the certificate
	// already installed on the instance is otherwise handed back untouched.
	// Both always travel together, a key without its certificate would leave
	// Pritunl unable to serve HTTPS at all.
	if cert != "" && key != "" {
		settings["server_cert"] = cert
		settings["server_key"] = key
	}

	portChanged := false

	if port, ok := d.GetOk("server_port"); ok {
		portChanged = port.(int) != settings.Int("server_port")
		settings["server_port"] = port.(int)
	}

	return portChanged
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

	settings, err := apiClient.GetSettings()
	if err != nil {
		return diag.FromErr(err)
	}

	// the settings object cannot be deleted, destroying the resource resets the
	// managed certificate instead. An empty string is enough to clear it:
	// Pritunl turns any falsy certificate into null and then regenerates its
	// self-signed default while restarting the web server. Every other setting,
	// server_port included, is handed back untouched by the round trip.
	settings["server_cert"] = ""
	settings["server_key"] = ""

	if err = apiClient.UpdateSettings(settings); err != nil {
		return diag.FromErr(err)
	}

	if err = waitForWebServer(ctx, apiClient); err != nil {
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
