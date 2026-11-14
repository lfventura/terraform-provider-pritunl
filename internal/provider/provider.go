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
				ValidateFunc: validateServerStartTimeout,
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

	serverStartTimeout, err := parseServerStartTimeout(d.Get("server_start_timeout").(string))
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

	return &providerMeta{Client: apiClient, serverStartTimeout: serverStartTimeout}, nil
}

const serverStartTimeoutDescription = "How long the start of a `pritunl_server` whose `status` is `online` may take, from the first start request until Pritunl reports the server running, as a Go duration greater than zero such as `90s` or `10m`. Within it the provider starts the server again when the start fails transiently or the server stops; when it closes with the server not running, the apply fails. Defaults to `5m`; also taken from the `PRITUNL_SERVER_START_TIMEOUT` environment variable."

// providerMeta is what the provider hands to the resources: the API client,
// which it embeds so that every resource reads it as a pritunl.Client, and
// the provider settings the resources need.
type providerMeta struct {
	pritunl.Client
	serverStartTimeout time.Duration
}

// serverStartTimeout is the window of a server start: the provider setting,
// or the default when the meta is a bare client, as in the unit tests.
func serverStartTimeout(meta interface{}) time.Duration {
	if m, ok := meta.(*providerMeta); ok && m.serverStartTimeout > 0 {
		return m.serverStartTimeout
	}

	return pritunl.DefaultServerStartTimeout
}

// parseServerStartTimeout reads server_start_timeout: a Go duration greater
// than zero.
func parseServerStartTimeout(value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("server_start_timeout %q is not a duration such as \"90s\" or \"10m\": %w", value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("server_start_timeout must be greater than zero, got %q", value)
	}

	return timeout, nil
}

func validateServerStartTimeout(value interface{}, _ string) ([]string, []error) {
	if _, err := parseServerStartTimeout(value.(string)); err != nil {
		return nil, []error{err}
	}

	return nil, nil
}
