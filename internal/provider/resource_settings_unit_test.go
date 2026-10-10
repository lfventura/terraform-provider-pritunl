package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
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
