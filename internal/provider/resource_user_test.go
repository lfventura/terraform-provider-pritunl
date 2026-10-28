package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPritunlUser(t *testing.T) {

	t.Run("creates users without error", func(t *testing.T) {
		username := "tfacc-user1"
		orgName := "tfacc-org1"

		check := resource.ComposeTestCheckFunc(
			resource.TestCheckResourceAttr("pritunl_user.test", "name", username),
			resource.TestCheckResourceAttr("pritunl_organization.test", "name", orgName),
			resource.TestCheckNoResourceAttr("pritunl_user.test", "pin"),
		)

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			Steps: []resource.TestStep{
				{
					Config: testPritunlUserConfig(username, orgName),
					Check:  check,
				},
				// import test
				pritunlUserImportStep("pritunl_user.test"),
			},
		})
	})
	t.Run("creates users with PIN without error", func(t *testing.T) {
		username := "tfacc-user2"
		orgName := "tfacc-org2"
		pin := "123456"

		check := resource.ComposeTestCheckFunc(
			resource.TestCheckResourceAttr("pritunl_user.test", "name", username),
			resource.TestCheckResourceAttr("pritunl_user.test", "pin", pin),
			resource.TestCheckResourceAttr("pritunl_organization.test", "name", orgName),
		)

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			Steps: []resource.TestStep{
				{
					Config: testPritunlUserConfigWithPin(username, orgName, pin),
					Check:  check,
				},
				// import test
				pritunlUserImportStep("pritunl_user.test"),
			},
		})
	})
	t.Run("sets the PIN from the write-only argument without storing it", func(t *testing.T) {
		username := "tfacc-user3"
		orgName := "tfacc-org3"

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			Steps: []resource.TestStep{
				{
					// write-only arguments need Terraform 1.11
					SkipFunc: terraformOlderThan(1, 11),
					Config: fmt.Sprintf(`
resource "pritunl_organization" "test" {
    name = "%[2]s"
}

resource "pritunl_user" "test" {
    name            = "%[1]s"
    organization_id = pritunl_organization.test.id
    pin_wo          = "654321"
    pin_wo_version  = 1
}
`, username, orgName),
					Check: resource.ComposeTestCheckFunc(
						resource.TestCheckNoResourceAttr("pritunl_user.test", "pin_wo"),
						resource.TestCheckNoResourceAttr("pritunl_user.test", "pin"),
						resource.TestCheckResourceAttr("pritunl_user.test", "pin_wo_version", "1"),
						testPritunlUserHasPin("pritunl_user.test"),
					),
				},
			},
		})
	})
}

func testPritunlUserConfig(username, orgName string) string {
	return testPritunlUserConfigWithPin(username, orgName, "")
}

func testPritunlUserConfigWithPin(username, orgName, pin string) string {
	resources := fmt.Sprintf(`
resource "pritunl_organization" "test" {
    name = "%[2]s"
}

resource "pritunl_user" "test" {
    name = "%[1]s"
    organization_id = pritunl_organization.test.id
    `, username, orgName)

	if pin != "" {
		resources += fmt.Sprintf("pin = \"%[1]s\"\n", pin)
	}

	resources += "}\n"

	return resources
}

// testPritunlUserHasPin checks on the Pritunl API that the user has a PIN, for
// a PIN the state never holds.
func testPritunlUserHasPin(name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		attributes := s.RootModule().Resources[name].Primary.Attributes
		user, err := testClient.GetUser(attributes["id"], attributes["organization_id"])
		if err != nil {
			return err
		}
		if user.Pin == nil || !user.Pin.IsSet {
			return fmt.Errorf("user %s has no PIN on the Pritunl API", attributes["id"])
		}
		return nil
	}
}

// terraformOlderThan skips a step on a Terraform CLI older than major.minor.
func terraformOlderThan(major, minor int) func() (bool, error) {
	return func() (bool, error) {
		out, err := exec.Command(terraformBinary(), "version", "-json").Output()
		if err != nil {
			return false, err
		}
		var version struct {
			TerraformVersion string `json:"terraform_version"`
		}
		if err := json.Unmarshal(out, &version); err != nil {
			return false, err
		}
		var gotMajor, gotMinor int
		if _, err := fmt.Sscanf(version.TerraformVersion, "%d.%d", &gotMajor, &gotMinor); err != nil {
			return false, err
		}
		return gotMajor < major || (gotMajor == major && gotMinor < minor), nil
	}
}

func terraformBinary() string {
	if path := os.Getenv("TF_ACC_TERRAFORM_PATH"); path != "" {
		return path
	}
	return "terraform"
}
