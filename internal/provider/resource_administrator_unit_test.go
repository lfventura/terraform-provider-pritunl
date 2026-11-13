package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// The schema of the resource has to hold together on its own: ConflictsWith
// pointing at an attribute that does not exist, or at a required one, is only
// caught here and not by the compiler.
func TestPritunlProviderSchema(t *testing.T) {
	if err := Provider().InternalValidate(); err != nil {
		t.Fatalf("the provider schema is invalid: %s", err)
	}
}

func TestAdministratorUsername(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"tfacc-admin", "tfacc-admin"},
		{"TFACC-Admin", "tfacc-admin"},
		{"terraform admin", "terraformadmin"},
		{"admin@example.com", "admin@example.com"},
		{" spaced ", "spaced"},
		{"quoted\"name", "quotedname"},
		{"", ""},
	}

	for _, testCase := range cases {
		if got := administratorUsername(testCase.configured); got != testCase.want {
			t.Errorf("administratorUsername(%q) = %q, want %q", testCase.configured, got, testCase.want)
		}
	}
}

func TestAdministratorYubikeyId(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		// a whole OTP, of which Pritunl keeps the public id only
		{"ccccccbcdefgtrljvnvtdjhbvrhlchgcbeghdthflukj", "ccccccbcdefg"},
		{"ccccccbcdefg", "ccccccbcdefg"},
		{" ccccccbcdefg ", "ccccccbcdefg"},
		{"cccccc", "cccccc"},
		{"", ""},
	}

	for _, testCase := range cases {
		if got := administratorYubikeyId(testCase.configured); got != testCase.want {
			t.Errorf("administratorYubikeyId(%q) = %q, want %q", testCase.configured, got, testCase.want)
		}
	}
}

// The guards Pritunl puts on the last super user of an instance cannot be
// tripped by the acceptance suite: they count the super users of the whole
// instance, and the account the suite authenticates as is one of them, so
// reaching them would mean demoting, disabling or deleting the very account
// every other test needs. What this provider owns of them is the diagnostic it
// builds out of the answer, and that is what is checked here, against the
// exact bodies the Pritunl backend sends.
func TestAdministratorDiagnostics(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		summary   string
		detail    string
		unchanged bool
	}{
		{
			name: "the last super user cannot be deleted",
			err: administratorApiError("deleting the administrator", 400, "no_admins",
				"At least one super administrator must exist.",
				`{"error": "no_admins", "error_msg": "At least one super administrator must exist."}`),
			summary: "At least one super administrator must exist.",
			detail:  "refuses to remove the last super user",
		},
		{
			name: "the last super user cannot be demoted",
			err: administratorApiError("updating the administrator", 400, "no_super_users",
				"There must be at least one super user.",
				`{"error": "no_super_users", "error_msg": "There must be at least one super user."}`),
			summary: "There must be at least one super user.",
			detail:  "refuses to take the super user flag off",
		},
		{
			name: "the last enabled super user cannot be disabled",
			err: administratorApiError("updating the administrator", 400, "no_admins_enabled",
				"At least one super administrator must be enabled.",
				`{"error": "no_admins_enabled", "error_msg": "At least one super administrator must be enabled."}`),
			summary: "At least one super administrator must be enabled.",
			detail:  "refuses to disable the last enabled super user",
		},
		{
			name: "the endpoint requires a super user",
			err: administratorApiError("getting the administrator", 400, "requires_super_user",
				"This administrator action can only be performed by a super user.",
				`{"error": "requires_super_user", "error_msg": "This administrator action can only be performed by a super user."}`),
			summary: "This administrator action can only be performed by a super user.",
			detail:  "requires a super user",
		},
		{
			name: "usernames are unique",
			err: administratorApiError("creating the administrator", 400, "admin_username_exists",
				"Administrator username already exists.",
				`{"error": "admin_username_exists", "error_msg": "Administrator username already exists."}`),
			summary: "Administrator username already exists.",
			detail:  "terraform import pritunl_administrator.example",
		},
		{
			name: "the two-step authentication modes are exclusive",
			err: administratorApiError("updating the administrator", 400, "admin_invalid_otp",
				"Cannot enable both local and authenticator two-step authentication.",
				`{"error": "admin_invalid_otp", "error_msg": "Cannot enable both local and authenticator two-step authentication."}`),
			summary: "Cannot enable both local and authenticator two-step authentication.",
			detail:  "never with both",
		},
		{
			name: "revoked credentials are explained",
			err:  administratorApiError("getting the administrator", 401, "", "", "401: Unauthorized"),
			// pritunl-web answers on its own here, with a body that carries no
			// error code at all
			summary: "The Pritunl API refused the administrator request",
			detail:  "the write revoked them",
		},
		{
			name: "an unknown refusal is reported as it is",
			err: administratorApiError("updating the administrator", 400, "something_else",
				"Something else went wrong.",
				`{"error": "something_else", "error_msg": "Something else went wrong."}`),
			summary:   "Non-200 response on updating the administrator",
			unchanged: true,
		},
		{
			name:      "an error that is not an api answer is reported as it is",
			err:       errors.New("UpdateAdministrator: Error on HTTP request: connection refused"),
			summary:   "UpdateAdministrator: Error on HTTP request: connection refused",
			unchanged: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			diagnostics := administratorDiagnostics(testCase.err)

			if len(diagnostics) != 1 {
				t.Fatalf("got %d diagnostics, want 1", len(diagnostics))
			}

			diagnostic := diagnostics[0]

			if testCase.unchanged {
				// diag.FromErr puts the whole error in the summary and leaves
				// the detail empty, which is what an answer this provider has
				// nothing to add to has to keep doing
				if diagnostic.Detail != "" {
					t.Errorf("got the detail %q, want none", diagnostic.Detail)
				}

				if !strings.Contains(diagnostic.Summary, testCase.summary) {
					t.Errorf("got the summary %q, want it to contain %q", diagnostic.Summary, testCase.summary)
				}

				return
			}

			if diagnostic.Summary != testCase.summary {
				t.Errorf("got the summary %q, want %q", diagnostic.Summary, testCase.summary)
			}

			if !strings.Contains(diagnostic.Detail, testCase.detail) {
				t.Errorf("got the detail %q, want it to contain %q", diagnostic.Detail, testCase.detail)
			}
		})
	}
}

// administratorApiError rebuilds the error the client hands over for a request
// the API refused. The parsing of the body the Pritunl backend answers with is
// covered by TestNewApiError in the pritunl package, here the fields it yields
// are what matters.
func administratorApiError(operation string, statusCode int, code, message, body string) error {
	err := &pritunl.ApiError{
		Operation:  operation,
		StatusCode: statusCode,
		Code:       code,
		Message:    message,
		Body:       body,
	}

	// the client hands the error over as it is, wrapping it here proves the
	// diagnostics look through a wrapped one as well
	return fmt.Errorf("the request failed: %w", err)
}

// administratorCreateClient is an instance holding a few administrators, as far
// as the create of pritunl_administrator is concerned: it answers the calls
// that path makes and panics on any other one through the nil embedded client.
type administratorCreateClient struct {
	pritunl.Client
	accounts []pritunl.Administrator
	created  []pritunl.Administrator
	updated  []pritunl.Administrator
}

func (c *administratorCreateClient) find(id string) pritunl.Administrator {
	for _, account := range c.accounts {
		if account.String("id") == id {
			return account
		}
	}

	return nil
}

func (c *administratorCreateClient) GetAdministrators() ([]pritunl.Administrator, error) {
	return c.accounts, nil
}

func (c *administratorCreateClient) GetAdministrator(id string) (pritunl.Administrator, error) {
	if account := c.find(id); account != nil {
		// a copy, the way a response is not the stored object
		read := pritunl.Administrator{}
		for key, value := range account {
			read[key] = value
		}

		return read, nil
	}

	return nil, pritunl.ErrAdministratorNotFound
}

func (c *administratorCreateClient) CreateAdministrator(administrator pritunl.Administrator) (pritunl.Administrator, error) {
	for _, account := range c.accounts {
		if account.String("username") == administrator.String("username") {
			return nil, administratorApiError("creating the administrator", 400, "admin_username_exists",
				"Administrator username already exists.",
				`{"error": "admin_username_exists", "error_msg": "Administrator username already exists."}`)
		}
	}

	sent := pritunl.Administrator{}
	for key, value := range administrator {
		sent[key] = value
	}

	c.created = append(c.created, sent)

	// the password is never part of what the instance reports
	account := pritunl.Administrator{"id": "6a9b8b734f5041e3afd7a11b"}
	for key, value := range administrator {
		if key != "password" {
			account[key] = value
		}
	}

	c.accounts = append(c.accounts, account)

	return account, nil
}

func (c *administratorCreateClient) UpdateAdministrator(id string, administrator pritunl.Administrator) error {
	sent := pritunl.Administrator{}
	for key, value := range administrator {
		sent[key] = value
	}

	c.updated = append(c.updated, sent)

	account := c.find(id)
	for key, value := range administrator {
		if key != "password" {
			account[key] = value
		}
	}

	return nil
}

// createAdministrator runs the create of the resource the way Terraform does,
// with the configuration as the raw configuration of the diff, which is the
// only place where an attribute written down is told from one left out.
func createAdministrator(t *testing.T, client pritunl.Client, config map[string]cty.Value) (*terraform.InstanceState, error) {
	t.Helper()

	resource := resourceAdministrator()

	raw := map[string]cty.Value{}
	configured := map[string]interface{}{}

	for name := range resource.Schema {
		raw[name] = cty.NullVal(cty.String)
	}

	for _, name := range administratorBoolAttributes {
		raw[name] = cty.NullVal(cty.Bool)
	}

	for name, value := range config {
		raw[name] = value

		switch value.Type() {
		case cty.Bool:
			configured[name] = value.True()
		default:
			configured[name] = value.AsString()
		}
	}

	diff, err := resource.Diff(context.Background(), nil, terraform.NewResourceConfigRaw(configured), client)
	if err != nil {
		t.Fatalf("planning the administrator failed: %s", err)
	}

	diff.RawConfig = cty.ObjectVal(raw)

	state, diagnostics := resource.Apply(context.Background(), nil, diff, client)
	if diagnostics.HasError() {
		return state, errors.New(diagnostics[0].Summary)
	}

	return state, nil
}

func TestCreateAdministratorWithoutPasswordGeneratesOne(t *testing.T) {
	client := &administratorCreateClient{}

	state, err := createAdministrator(t, client, map[string]cty.Value{
		"username": cty.StringVal("operator"),
	})
	if err != nil {
		t.Fatalf("expected the create to succeed, got %s", err)
	}

	if len(client.created) != 1 {
		t.Fatalf("expected one create request, got %d", len(client.created))
	}

	sent := client.created[0].String("password")
	if len(sent) < 32 {
		t.Fatalf("expected a generated password of at least 32 characters, sent %d", len(sent))
	}

	if password := state.Attributes["password"]; password != "" {
		t.Fatalf("expected the password to stay out of the state, got %d characters", len(password))
	}

	for key, value := range state.Attributes {
		if sent != "" && strings.Contains(value, sent) {
			t.Fatalf("the generated password reached the state attribute %q", key)
		}
	}

	if len(client.updated) != 0 {
		t.Fatalf("expected no update of a new account, got %d", len(client.updated))
	}
}

func TestCreateAdministratorWithPasswordSendsIt(t *testing.T) {
	client := &administratorCreateClient{}

	state, err := createAdministrator(t, client, map[string]cty.Value{
		"username": cty.StringVal("operator"),
		"password": cty.StringVal("configured-secret"),
	})
	if err != nil {
		t.Fatalf("expected the create to succeed, got %s", err)
	}

	if sent := client.created[0].String("password"); sent != "configured-secret" {
		t.Fatalf("expected the configured password to be sent, sent %q", sent)
	}

	if password := state.Attributes["password"]; password != "configured-secret" {
		t.Fatalf("expected the configured password in the state, got %q", password)
	}
}

func TestGenerateAdministratorPassword(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 50; i++ {
		password, err := generateAdministratorPassword()
		if err != nil {
			t.Fatalf("expected a password, got %s", err)
		}

		if len(password) < 32 {
			t.Fatalf("expected at least 32 characters, got %d", len(password))
		}

		for _, class := range []string{administratorPasswordLetters, administratorPasswordDigits, administratorPasswordSymbols} {
			if !strings.ContainsAny(password, class) {
				t.Fatalf("expected a character of %q in the password", class)
			}
		}

		if seen[password] {
			t.Fatalf("expected passwords to differ, got a repeat")
		}

		seen[password] = true
	}
}

func TestCreateAdministratorAdoptsAnExistingAccount(t *testing.T) {
	client := &administratorCreateClient{
		accounts: []pritunl.Administrator{{
			"id":             "5f1b2a3c4d5e6f7a8b9c0d1e",
			"username":       "operator",
			"super_user":     false,
			"auth_api":       false,
			"disabled":       true,
			"otp_auth":       false,
			"local_otp_auth": false,
			"yubikey_id":     "",
			"theme":          "dark",
		}},
	}

	state, err := createAdministrator(t, client, map[string]cty.Value{
		"username":   cty.StringVal("Operator"),
		"password":   cty.StringVal("configured-secret"),
		"super_user": cty.BoolVal(true),
		"auth_api":   cty.BoolVal(true),
		"disabled":   cty.BoolVal(false),
		"otp_auth":   cty.BoolVal(true),
		"yubikey_id": cty.StringVal("ccccccbcdefgtrljvnvtdjhbvrhlchgcbeghdthflukj"),
	})
	if err != nil {
		t.Fatalf("expected the create to adopt the account, got %s", err)
	}

	if state.ID != "5f1b2a3c4d5e6f7a8b9c0d1e" {
		t.Fatalf("expected the id of the existing account, got %q", state.ID)
	}

	if len(client.created) != 0 || len(client.accounts) != 1 {
		t.Fatalf("expected no second account, got %d created and %d held", len(client.created), len(client.accounts))
	}

	if len(client.updated) != 1 {
		t.Fatalf("expected one update converging the account, got %d", len(client.updated))
	}

	updated := client.updated[0]

	for attribute, want := range map[string]bool{"super_user": true, "auth_api": true, "disabled": false, "otp_auth": true, "local_otp_auth": false} {
		if got := updated.Bool(attribute); got != want {
			t.Errorf("expected %s=%t on the adopted account, sent %t", attribute, want, got)
		}
	}

	if got := updated.String("yubikey_id"); got != "ccccccbcdefg" {
		t.Errorf("expected the yubikey id to be converged, sent %q", got)
	}

	if got := updated.String("theme"); got != "dark" {
		t.Errorf("expected what the resource does not manage to be handed back untouched, sent %q", got)
	}

	if got := updated.String("password"); got != "configured-secret" {
		t.Errorf("expected the configured password to be written, sent %q", got)
	}

	if state.Attributes["super_user"] != "true" || state.Attributes["disabled"] != "false" {
		t.Errorf("expected the state to hold the converged account, got %v", state.Attributes)
	}
}

func TestCreateAdministratorAdoptsWithoutAPasswordWhenNoneIsConfigured(t *testing.T) {
	client := &administratorCreateClient{
		accounts: []pritunl.Administrator{{
			"id":             "5f1b2a3c4d5e6f7a8b9c0d1e",
			"username":       "operator",
			"super_user":     true,
			"auth_api":       false,
			"disabled":       false,
			"otp_auth":       false,
			"local_otp_auth": false,
		}},
	}

	state, err := createAdministrator(t, client, map[string]cty.Value{
		"username":   cty.StringVal("operator"),
		"auth_api":   cty.BoolVal(true),
		"super_user": cty.BoolVal(true),
	})
	if err != nil {
		t.Fatalf("expected the create to adopt the account, got %s", err)
	}

	if len(client.updated) != 1 {
		t.Fatalf("expected one update, got %d", len(client.updated))
	}

	if client.updated[0].Has("password") {
		t.Fatalf("expected no password to be sent for an adopted account")
	}

	if !client.updated[0].Bool("auth_api") {
		t.Fatalf("expected auth_api to be converged")
	}

	if password := state.Attributes["password"]; password != "" {
		t.Fatalf("expected the password to stay out of the state, got %d characters", len(password))
	}
}
