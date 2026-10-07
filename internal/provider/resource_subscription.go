package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

const subscriptionResourceId = "subscription"

func resourceSubscription() *schema.Resource {
	return &schema.Resource{
		Description: "The subscription resource activates a Pritunl license on the instance. " +
			"It is a singleton: one instance of it maps to the whole Pritunl cluster. " +
			"The license key is write-only — the Pritunl API never returns it — so the key " +
			"lives in the Terraform state as a sensitive value and a changed key replaces " +
			"the resource. Activation is validated by the Pritunl host against " +
			"app.pritunl.com, which the host must be able to reach.",
		Schema: map[string]*schema.Schema{
			"license": {
				Type:        schema.TypeString,
				Required:    true,
				Sensitive:   true,
				ForceNew:    true,
				Description: "The Pritunl license key to activate. Write-only on the Pritunl API; a changed key replaces the resource.",
			},
			"status": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "The subscription status the instance reports.",
			},
			"plan": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "The subscription plan the instance reports.",
			},
			"quantity": {
				Type:        schema.TypeInt,
				Computed:    true,
				Description: "The licensed quantity the instance reports.",
			},
		},
		CreateContext: resourceCreateSubscription,
		ReadContext:   resourceReadSubscription,
		DeleteContext: resourceDeleteSubscription,
	}
}

func resourceCreateSubscription(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	if err := apiClient.ActivateSubscription(d.Get("license").(string)); err != nil {
		return diag.FromErr(err)
	}

	d.SetId(subscriptionResourceId)

	return resourceReadSubscription(ctx, d, meta)
}

func resourceReadSubscription(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	subscription, err := apiClient.GetSubscription()
	if err != nil {
		return diag.FromErr(err)
	}

	if !subscription.Active {
		// the license is gone on the instance; recreating re-activates it
		d.SetId("")
		return nil
	}

	d.Set("status", subscription.Status)
	d.Set("plan", subscription.Plan)
	d.Set("quantity", subscription.Quantity)

	return nil
}

func resourceDeleteSubscription(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	apiClient := meta.(pritunl.Client)

	if err := apiClient.DeleteSubscription(); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")

	return nil
}
