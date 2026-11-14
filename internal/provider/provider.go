package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

func Provider() *schema.Provider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"url": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("PRITUNL_URL", ""),
			},
			"token": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("PRITUNL_TOKEN", ""),
			},
			"secret": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("PRITUNL_SECRET", ""),
			},
			"insecure": {
				Type:        schema.TypeBool,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("PRITUNL_INSECURE", false),
			},
			"connection_check": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PRITUNL_CONNECTION_CHECK", true),
			},
			"server_start_timeout": {
				Type:         schema.TypeString,
				Optional:     true,
				Description:  serverStartTimeoutDescription,
				DefaultFunc:  schema.EnvDefaultFunc("PRITUNL_SERVER_START_TIMEOUT", pritunl.DefaultServerStartTimeout.String()),
				ValidateFunc: validateDurationSetting("server_start_timeout"),
			},
			"server_dh_timeout": {
				Type:         schema.TypeString,
				Optional:     true,
				Description:  serverDhTimeoutDescription,
				DefaultFunc:  schema.EnvDefaultFunc("PRITUNL_SERVER_DH_TIMEOUT", pritunl.DefaultServerDhTimeout.String()),
				ValidateFunc: validateDurationSetting("server_dh_timeout"),
			},
		},
		ResourcesMap: map[string]*schema.Resource{
			"pritunl_administrator": resourceAdministrator(),
			"pritunl_organization":  resourceOrganization(),
			"pritunl_server":        resourceServer(),
			"pritunl_settings":      resourceSettings(),
			"pritunl_subscription":  resourceSubscription(),
			"pritunl_host":          resourceHost(),
			"pritunl_user":          resourceUser(),
		},
		DataSourcesMap: map[string]*schema.Resource{
			"pritunl_host":  dataSourceHost(),
			"pritunl_hosts": dataSourceHosts(),
		},
		ConfigureContextFunc: providerConfigure,
	}
}

func providerConfigure(ctx context.Context, d *schema.ResourceData) (interface{}, diag.Diagnostics) {
	url := d.Get("url").(string)
	token := d.Get("token").(string)
	secret := d.Get("secret").(string)
	insecure := d.Get("insecure").(bool)
	connectionCheck := d.Get("connection_check").(bool)

	serverStartTimeout, err := parseDurationSetting("server_start_timeout", d.Get("server_start_timeout").(string))
	if err != nil {
		return nil, diag.FromErr(err)
	}
	serverDhTimeout, err := parseDurationSetting("server_dh_timeout", d.Get("server_dh_timeout").(string))
	if err != nil {
		return nil, diag.FromErr(err)
	}

	apiClient := pritunl.NewClient(url, token, secret, insecure)

	if connectionCheck {
		// execute test api call to ensure that provided credentials are valid and pritunl api works
		err := apiClient.TestApiCall()
		if err != nil {
			return nil, diag.FromErr(err)
		}
	}

	return &providerMeta{Client: apiClient, serverStartTimeout: serverStartTimeout, serverDhTimeout: serverDhTimeout}, nil
}

const serverStartTimeoutDescription = "How long the start of a `pritunl_server` whose `status` is `online` may take, from the moment the server has its DH parameters until Pritunl reports it running, as a Go duration greater than zero such as `90s` or `10m`. Within it the provider starts the server again when a start fails or the server stops; every request of the start ends when the window closes, and when it closes with the server not running, the apply fails. Defaults to `5m`; also taken from the `PRITUNL_SERVER_START_TIMEOUT` environment variable."

const serverDhTimeoutDescription = "How long the provider waits for a `pritunl_server` whose `status` is `online` to get its DH parameters before it starts the server, as a Go duration greater than zero such as `90s` or `15m`. Pritunl keeps 3 pre-generated DH parameter sets; a server created when none is left reads `pending` until a set is generated for it, which takes from seconds to several minutes depending on the CPU. The provider sends no start while the server reads `pending`. When the window closes with the server still pending, the apply fails with an error that names the server; a server being created is then tainted, and the next apply replaces it. Defaults to `10m`; also taken from the `PRITUNL_SERVER_DH_TIMEOUT` environment variable."

// providerMeta is what the provider hands to the resources: the API client,
// which it embeds so that every resource reads it as a pritunl.Client, and
// the provider settings the resources need.
type providerMeta struct {
	pritunl.Client
	serverStartTimeout time.Duration
	serverDhTimeout    time.Duration
}

// serverStartTimeout is the window of a server start: the provider setting,
// or the default when the meta is a bare client, as in the unit tests.
func serverStartTimeout(meta interface{}) time.Duration {
	if m, ok := meta.(*providerMeta); ok && m.serverStartTimeout > 0 {
		return m.serverStartTimeout
	}

	return pritunl.DefaultServerStartTimeout
}

// serverDhTimeout is how long a pending server is waited for: the provider
// setting, or the default when the meta is a bare client.
func serverDhTimeout(meta interface{}) time.Duration {
	if m, ok := meta.(*providerMeta); ok && m.serverDhTimeout > 0 {
		return m.serverDhTimeout
	}

	return pritunl.DefaultServerDhTimeout
}

// parseDurationSetting reads a provider setting that is a Go duration greater
// than zero.
func parseDurationSetting(name, value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration such as \"90s\" or \"10m\": %w", name, value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %q", name, value)
	}

	return timeout, nil
}

func validateDurationSetting(name string) schema.SchemaValidateFunc {
	return func(value interface{}, _ string) ([]string, []error) {
		if _, err := parseDurationSetting(name, value.(string)); err != nil {
			return nil, []error{err}
		}

		return nil, nil
	}
}
