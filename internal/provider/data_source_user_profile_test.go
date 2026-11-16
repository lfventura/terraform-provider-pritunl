package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccPritunlUserProfile(t *testing.T) {
	pem := regexp.MustCompile(`^-----BEGIN [A-Z ]+-----\n(?s:.+)\n-----END [A-Z ]+-----\n$`)
	// Pritunl writes the key with the comment header openvpn --genkey adds
	staticKey := regexp.MustCompile(`^(?:#.*\n)*-----BEGIN OpenVPN Static key V1-----\n(?s:.+)\n-----END OpenVPN Static key V1-----\n$`)

	check := resource.ComposeTestCheckFunc(
		resource.TestMatchResourceAttr("data.pritunl_user_profile.test", "ca_cert", pem),
		resource.TestMatchResourceAttr("data.pritunl_user_profile.test", "user_cert", pem),
		resource.TestMatchResourceAttr("data.pritunl_user_profile.test", "user_key", pem),
		resource.TestMatchResourceAttr("data.pritunl_user_profile.test", "tls_auth_key", staticKey),
		resource.TestCheckResourceAttr("data.pritunl_user_profile.test", "remotes.#", "1"),
		resource.TestCheckResourceAttrPair("data.pritunl_user_profile.test", "remotes.0.port", "pritunl_server.test", "port"),
	)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { preCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testPritunlUserProfileConfig("tfacc-profile-org", "tfacc-profile-user", "tfacc-profile-server"),
				Check:  check,
			},
			{
				// a second read of the same user and server must not change
				// the values: the per-download lines are dropped
				Config:   testPritunlUserProfileConfig("tfacc-profile-org", "tfacc-profile-user", "tfacc-profile-server"),
				PlanOnly: true,
			},
		},
	})
}

func testPritunlUserProfileConfig(orgName, userName, serverName string) string {
	return fmt.Sprintf(`
resource "pritunl_organization" "test" {
	name = "%[1]s"
}

resource "pritunl_user" "test" {
	name            = "%[2]s"
	organization_id = pritunl_organization.test.id
}

resource "pritunl_server" "test" {
	name             = "%[3]s"
	organization_ids = [pritunl_organization.test.id]
}

data "pritunl_user_profile" "test" {
	organization_id = pritunl_organization.test.id
	user_id         = pritunl_user.test.id
	server_id       = pritunl_server.test.id
}
`, orgName, userName, serverName)
}
