package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

func dataSourceUserProfile() *schema.Resource {
	return &schema.Resource{
		Description: "The OpenVPN connection material of a user on a server: the server CA, the user's certificate and key, the tls-auth key and the remotes. It is the content of the profile Pritunl serves for the user, without the lines Pritunl changes on every download, so the values only change when the user, the server or its keys do. Use it to hand a non-interactive client (a CI runner) its connection secrets.",
		ReadContext: dataSourceUserProfileRead,
		Schema: map[string]*schema.Schema{
			"organization_id": {
				Description: "Organization of the user.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"user_id": {
				Description: "User whose profile is read.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"server_id": {
				Description: "Server the profile connects to; the organization must be attached to it.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"ca_cert": {
				Description: "PEM certificate of the server CA.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"user_cert": {
				Description: "PEM certificate of the user.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"user_key": {
				Description: "PEM private key of the user.",
				Type:        schema.TypeString,
				Computed:    true,
				Sensitive:   true,
			},
			"tls_auth_key": {
				Description: "OpenVPN static key of the server's tls-auth; empty when the server uses none.",
				Type:        schema.TypeString,
				Computed:    true,
				Sensitive:   true,
			},
			"key_direction": {
				Description: "The key-direction the profile sets for the tls-auth key.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"remotes": {
				Description: "The remote lines of the profile, in order.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"host": {
							Description: "Address or name the client connects to.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"port": {
							Description: "Port the client connects to.",
							Type:        schema.TypeInt,
							Computed:    true,
						},
						"protocol": {
							Description: "Protocol of the remote line; empty when the line sets none.",
							Type:        schema.TypeString,
							Computed:    true,
						},
					},
				},
			},
		},
	}
}

func dataSourceUserProfileRead(_ context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	orgId := d.Get("organization_id").(string)
	userId := d.Get("user_id").(string)
	serverId := d.Get("server_id").(string)

	profile, err := apiClient.GetUserProfile(orgId, userId, serverId)
	if err != nil {
		return diag.FromErr(err)
	}

	remotes := make([]map[string]interface{}, 0, len(profile.Remotes))
	for _, r := range profile.Remotes {
		remotes = append(remotes, map[string]interface{}{
			"host":     r.Host,
			"port":     r.Port,
			"protocol": r.Protocol,
		})
	}

	d.SetId(fmt.Sprintf("%s-%s-%s", orgId, userId, serverId))
	d.Set("ca_cert", profile.CACert)
	d.Set("user_cert", profile.UserCert)
	d.Set("user_key", profile.UserKey)
	d.Set("tls_auth_key", profile.TLSAuthKey)
	d.Set("key_direction", profile.KeyDirection)
	if err := d.Set("remotes", remotes); err != nil {
		return diag.FromErr(err)
	}

	return nil
}
