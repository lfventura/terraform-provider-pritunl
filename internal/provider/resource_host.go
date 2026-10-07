package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

func resourceHost() *schema.Resource {
	return &schema.Resource{
		Description: "The host resource manages the settings of a Pritunl instance's host. " +
			"Hosts are born when the pritunl agent registers, never created by Terraform: " +
			"this resource adopts the instance's single host and manages its settings " +
			"(the display name and the public address clients connect to). An instance " +
			"running more than one host is not supported and fails naming them. " +
			"Destroying the resource only forgets it — the host itself is never removed.",
		Schema: map[string]*schema.Schema{
			"hostname": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "The machine hostname the adopted host registered with.",
			},
			"name": {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				Description: "The host's display name. Unset keeps the name the host already carries.",
			},
			"public_address": {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				Description: "The public address clients connect to. Unset keeps the address the host already carries (auto-detected when never configured).",
			},
		},
		CreateContext: resourceCreateHost,
		ReadContext:   resourceReadHost,
		UpdateContext: resourceUpdateHost,
		DeleteContext: resourceDeleteHost,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
	}
}

func resourceCreateHost(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	hosts, err := apiClient.GetHosts()
	if err != nil {
		return diag.FromErr(err)
	}

	if len(hosts) == 0 {
		return diag.Errorf("no host is registered on the instance")
	}
	if len(hosts) > 1 {
		hostnames := make([]string, 0, len(hosts))
		for _, host := range hosts {
			hostnames = append(hostnames, host.Hostname)
		}
		return diag.Errorf("the instance runs %d hosts (%s); this resource manages single-host instances only", len(hosts), strings.Join(hostnames, ", "))
	}

	d.SetId(hosts[0].ID)

	return writeHostSettings(ctx, d, meta)
}

func resourceUpdateHost(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	return writeHostSettings(ctx, d, meta)
}

// writeHostSettings overlays the configured settings on the ones the host
// already carries and sends them together, so an unset attribute never
// clears the other.
func writeHostSettings(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	host, err := apiClient.GetHost(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	if host == nil {
		return diag.FromErr(fmt.Errorf("host %s is gone from the instance", d.Id()))
	}

	update := pritunl.HostUpdate{
		Name:          host.Name,
		PublicAddress: host.PublicAddress,
	}
	if v, ok := d.GetOk("name"); ok {
		update.Name = v.(string)
	}
	if v, ok := d.GetOk("public_address"); ok {
		update.PublicAddress = v.(string)
	}

	if _, err := apiClient.UpdateHost(d.Id(), update); err != nil {
		return diag.FromErr(err)
	}

	return resourceReadHost(ctx, d, meta)
}

func resourceReadHost(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	host, err := apiClient.GetHost(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	if host == nil {
		d.SetId("")
		return nil
	}

	d.Set("hostname", host.Hostname)
	d.Set("name", host.Name)
	d.Set("public_address", host.PublicAddress)

	return nil
}

func resourceDeleteHost(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	// hosts belong to the agents that registered them: the resource is
	// forgotten, the host stays
	d.SetId("")

	return nil
}
