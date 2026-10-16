package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

func TestNormalizeSAMLCert(t *testing.T) {
	naked := "MIIDqDCCApCgAwIBAgIGAZkxg7q2MA0GCSqGSIb3DQEBCwUAMIGUMQswCQYDVQQGEwJVUzETMBEGA1UECAwKQ2FsaWZvcm5pYTEWMBQGA1UEBwwNU2FuIEZyYW5jaXNjbw"
	armored := "-----BEGIN CERTIFICATE-----\n" + naked[:64] + "\n" + naked[64:128] + "\n" + naked[128:] + "\n-----END CERTIFICATE-----"

	cases := map[string]struct {
		in   string
		want string
	}{
		"empty stays empty":              {"", ""},
		"whitespace only stays empty":    {" \n\t ", ""},
		"naked base64 gets armored":      {naked, armored},
		"metadata line breaks are fine":  {naked[:40] + "\n" + naked[40:80] + " \n " + naked[80:], armored},
		"armored passes through trimmed": {"\n" + armored + "\n", armored},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := normalizeSAMLCert(c.in)
			if got != c.want {
				t.Fatalf("normalizeSAMLCert(%q):\n got %q\nwant %q", c.in, got, c.want)
			}
			if again := normalizeSAMLCert(got); again != got {
				t.Fatalf("not idempotent:\nfirst %q\n then %q", got, again)
			}
			for i, line := range strings.Split(got, "\n") {
				if len(line) > 64 {
					t.Fatalf("line %d longer than 64 columns: %q", i+1, line)
				}
			}
		})
	}
}

// ssoRawConfig builds the raw configuration object validateSsoRawConfig reads,
// with every single sign-on attribute null unless overridden: cty.GetAttr
// panics on a missing attribute, and a real raw configuration always carries
// the whole schema.
func ssoRawConfig(overrides map[string]cty.Value) cty.Value {
	values := map[string]cty.Value{"sso": cty.NullVal(cty.String), "sso_okta_mode": cty.NullVal(cty.String)}
	for _, key := range settingsSsoAttributes {
		values[key] = cty.NullVal(cty.String)
	}

	for key, value := range overrides {
		values[key] = value
	}

	return cty.ObjectVal(values)
}

func TestValidateSsoRawConfig(t *testing.T) {
	cases := map[string]struct {
		config  cty.Value
		wantErr string
	}{
		"absent sso is never validated": {
			config: ssoRawConfig(map[string]cty.Value{"sso_org": cty.StringVal("5f1b2a")}),
		},
		"unknown sso is left to the apply": {
			config: ssoRawConfig(map[string]cty.Value{"sso": cty.UnknownVal(cty.String)}),
		},
		"saml_okta with its companions": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":            cty.StringVal(ssoSamlOkta),
				"sso_org":        cty.StringVal("5f1b2a"),
				"server_sso_url": cty.StringVal("https://vpn.example.com"),
			}),
		},
		"saml_okta with an organization still being created": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":            cty.StringVal(ssoSamlOkta),
				"sso_org":        cty.UnknownVal(cty.String),
				"server_sso_url": cty.StringVal("https://vpn.example.com"),
			}),
		},
		"saml_okta without the organization": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":            cty.StringVal(ssoSamlOkta),
				"server_sso_url": cty.StringVal("https://vpn.example.com"),
			}),
			wantErr: "sso_org",
		},
		"saml_okta with a blank organization": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":            cty.StringVal(ssoSamlOkta),
				"sso_org":        cty.StringVal("  "),
				"server_sso_url": cty.StringVal("https://vpn.example.com"),
			}),
			wantErr: "sso_org",
		},
		"saml_okta without the domain": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":     cty.StringVal(ssoSamlOkta),
				"sso_org": cty.StringVal("5f1b2a"),
			}),
			wantErr: "server_sso_url",
		},
		"a value the schema never accepts": {
			config:  ssoRawConfig(map[string]cty.Value{"sso": cty.StringVal("saml_okta_duo")}),
			wantErr: "must be",
		},
		"disabled on its own": {
			config: ssoRawConfig(map[string]cty.Value{"sso": cty.StringVal(ssoDisabled)}),
		},
		"disabled refuses a companion": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":     cty.StringVal(ssoDisabled),
				"sso_org": cty.StringVal("5f1b2a"),
			}),
			wantErr: "sso_org",
		},
		"disabled refuses the empty okta mode": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":           cty.StringVal(ssoDisabled),
				"sso_okta_mode": cty.StringVal(""),
			}),
			wantErr: "sso_okta_mode",
		},
		"disabled refuses an unknown companion": {
			config: ssoRawConfig(map[string]cty.Value{
				"sso":     cty.StringVal(ssoDisabled),
				"sso_org": cty.UnknownVal(cty.String),
			}),
			wantErr: "sso_org",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateSsoRawConfig(c.config)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSsoRawConfig: unexpected error %q", err)
				}

				return
			}
			if err == nil {
				t.Fatalf("validateSsoRawConfig: want an error naming %q, got none", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("validateSsoRawConfig: the error %q does not name %q", err, c.wantErr)
			}
		})
	}
}

func TestSsoValidateFunc(t *testing.T) {
	validate := resourceSettings().Schema["sso"].ValidateFunc

	for _, value := range []string{ssoSamlOkta, ssoDisabled} {
		if _, errs := validate(value, "sso"); len(errs) > 0 {
			t.Fatalf("sso rejects %q: %v", value, errs)
		}
	}

	for _, value := range []string{"", "okta", "saml_okta_duo"} {
		if _, errs := validate(value, "sso"); len(errs) == 0 {
			t.Fatalf("sso accepts %q, want a validation error", value)
		}
	}
}

func TestValidateAuditingRawConfig(t *testing.T) {
	config := func(auditing cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"auditing": auditing})
	}

	cases := map[string]struct {
		enabled bool
		config  cty.Value
		wantErr bool
	}{
		"on to off is refused":             {enabled: true, config: config(cty.False), wantErr: true},
		"on stays on":                      {enabled: true, config: config(cty.True)},
		"on and unset keeps it as it is":   {enabled: true, config: config(cty.NullVal(cty.Bool))},
		"on and unknown is left alone":     {enabled: true, config: config(cty.UnknownVal(cty.Bool))},
		"off to on":                        {enabled: false, config: config(cty.True)},
		"off stays off":                    {enabled: false, config: config(cty.False)},
		"off and unset keeps it as it is":  {enabled: false, config: config(cty.NullVal(cty.Bool))},
		"a null configuration is not read": {enabled: true, config: cty.NullVal(cty.Object(map[string]cty.Type{"auditing": cty.Bool}))},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateAuditingRawConfig(c.enabled, c.config)
			if c.wantErr && err == nil {
				t.Fatal("validateAuditingRawConfig: want an error, got none")
			}
			if c.wantErr && !strings.Contains(err.Error(), "auditing") {
				t.Fatalf("validateAuditingRawConfig: the error %q does not name auditing", err)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateAuditingRawConfig: unexpected error %q", err)
			}
		})
	}
}

// settingsResourceData builds the resource data of a pritunl_settings being
// planned from the given configuration, with the raw configuration the
// overlay reads attached the way the plugin server attaches it: every
// attribute the configuration does not mention is null.
func settingsResourceData(t *testing.T, raw map[string]interface{}, rawConfig map[string]cty.Value) *schema.ResourceData {
	t.Helper()

	resource := resourceSettings()
	sm := schema.InternalMap(resource.Schema)

	values := map[string]cty.Value{}
	for key, attribute := range sm.CoreConfigSchema().Attributes {
		values[key] = cty.NullVal(attribute.Type)
	}
	values["id"] = cty.NullVal(cty.String)
	for key, value := range rawConfig {
		values[key] = value
	}

	diff, err := sm.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(raw), nil, nil, true)
	if err != nil {
		t.Fatalf("failed to plan the configuration: %s", err)
	}
	diff.RawConfig = cty.ObjectVal(values)

	data, err := sm.Data(nil, diff)
	if err != nil {
		t.Fatalf("failed to build the resource data: %s", err)
	}

	return data
}

func TestOverlaySettingsAuditing(t *testing.T) {
	cases := map[string]struct {
		instance  interface{}
		raw       map[string]interface{}
		rawConfig map[string]cty.Value
		want      interface{}
	}{
		"true turns it on": {
			instance:  nil,
			raw:       map[string]interface{}{"auditing": true},
			rawConfig: map[string]cty.Value{"auditing": cty.True},
			want:      pritunl.AuditingAll,
		},
		"true keeps it on": {
			instance:  pritunl.AuditingAll,
			raw:       map[string]interface{}{"auditing": true},
			rawConfig: map[string]cty.Value{"auditing": cty.True},
			want:      pritunl.AuditingAll,
		},
		"false keeps it off as null": {
			instance:  nil,
			raw:       map[string]interface{}{"auditing": false},
			rawConfig: map[string]cty.Value{"auditing": cty.False},
			want:      nil,
		},
		"unset hands an instance that is on back untouched": {
			instance:  pritunl.AuditingAll,
			raw:       map[string]interface{}{"pin_mode": "required"},
			rawConfig: map[string]cty.Value{"pin_mode": cty.StringVal("required")},
			want:      pritunl.AuditingAll,
		},
		"unset hands an instance that is off back untouched": {
			instance:  nil,
			raw:       map[string]interface{}{"pin_mode": "required"},
			rawConfig: map[string]cty.Value{"pin_mode": cty.StringVal("required")},
			want:      nil,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := settingsResourceData(t, c.raw, c.rawConfig)
			settings := pritunl.Settings{"auditing": c.instance}

			overlaySettings(d, settings)

			if got := settings["auditing"]; got != c.want {
				t.Fatalf("auditing after the overlay: got %#v, want %#v", got, c.want)
			}
		})
	}
}

// planSettings plans the given configuration against a pritunl_settings
// state through the resource's own CustomizeDiff, the way the plugin server
// plans it: the raw configuration carries every attribute of the schema, null
// where the configuration does not mention it, and rides on the prior state.
func planSettings(t *testing.T, state map[string]string, rawConfig map[string]cty.Value) error {
	t.Helper()

	resource := resourceSettings()
	coreSchema := resource.CoreConfigSchema()

	values := map[string]cty.Value{}
	for key, attribute := range coreSchema.Attributes {
		values[key] = cty.NullVal(attribute.Type)
	}
	for key, value := range rawConfig {
		values[key] = value
	}

	configVal := cty.ObjectVal(values)
	config := terraform.NewResourceConfigShimmed(configVal, coreSchema)
	instance := &terraform.InstanceState{ID: "settings", Attributes: state, RawConfig: configVal}

	_, err := schema.InternalMap(resource.Schema).Diff(context.Background(), instance, config, resource.CustomizeDiff, nil, true)

	return err
}

func TestCustomizeSettingsDiffAuditing(t *testing.T) {
	cases := map[string]struct {
		state     map[string]string
		rawConfig map[string]cty.Value
		wantErr   bool
	}{
		"on to off fails the plan": {
			state:     map[string]string{"id": "settings", "auditing": "true"},
			rawConfig: map[string]cty.Value{"auditing": cty.False},
			wantErr:   true,
		},
		"off to on is planned": {
			state:     map[string]string{"id": "settings", "auditing": "false"},
			rawConfig: map[string]cty.Value{"auditing": cty.True},
		},
		"on and unset is planned": {
			state:     map[string]string{"id": "settings", "auditing": "true"},
			rawConfig: map[string]cty.Value{"pin_mode": cty.StringVal("required")},
		},
		"on stays on": {
			state:     map[string]string{"id": "settings", "auditing": "true"},
			rawConfig: map[string]cty.Value{"auditing": cty.True},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := planSettings(t, c.state, c.rawConfig)
			if c.wantErr && err == nil {
				t.Fatal("plan: want an error, got none")
			}
			if c.wantErr && !strings.Contains(err.Error(), "auditing") {
				t.Fatalf("plan: the error %q does not name auditing", err)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("plan: unexpected error %q", err)
			}
		})
	}
}

// TestOverlaySettingsServerPortDefault: a configuration that leaves
// server_port out holds the instance at 443, writing it over another port.
func TestOverlaySettingsServerPortDefault(t *testing.T) {
	d := settingsResourceData(t, map[string]interface{}{}, nil)
	// the instance answers with a JSON number, as the API does
	settings := pritunl.Settings{"server_port": json.Number("8443")}

	if changed := overlaySettings(d, settings); !changed {
		t.Fatal("overlaySettings reported the port as unchanged, expected 8443 -> 443 to be a change")
	}
	if got := settings["server_port"]; got != 443 {
		t.Fatalf("server_port written as %v, expected the default 443", got)
	}
}
