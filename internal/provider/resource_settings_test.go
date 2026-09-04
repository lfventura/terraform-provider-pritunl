package provider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// This test pushes a certificate to the very same Pritunl instance the whole
// acceptance suite runs against, which makes Pritunl restart the web server
// serving its API. A malformed or mismatched pair would leave that instance
// without a usable HTTPS endpoint and break every other test, so the pair is
// generated as a valid self-signed certificate, the provider waits for the web
// server to come back after each write, and destroying the resource hands the
// certificate back to Pritunl, which regenerates its self-signed default.
// Setting PRITUNL_SKIP_SETTINGS_ACC_TEST opts out of it without touching the
// rest of the suite.
func TestAccPritunlSettings(t *testing.T) {
	if os.Getenv("PRITUNL_SKIP_SETTINGS_ACC_TEST") != "" {
		t.Skip("PRITUNL_SKIP_SETTINGS_ACC_TEST is set, skipping the pritunl_settings acceptance test")
	}

	t.Run("applies a certificate without error", func(t *testing.T) {
		cert, key := generateSelfSignedCertificate(t)

		check := resource.ComposeTestCheckFunc(
			resource.TestCheckResourceAttr("pritunl_settings.test", "id", "settings"),
			resource.TestCheckResourceAttr("pritunl_settings.test", "server_cert", cert),
			resource.TestCheckResourceAttr("pritunl_settings.test", "server_key", key),
			resource.TestCheckResourceAttrSet("pritunl_settings.test", "server_port"),
			testAccCheckSettingsCertificate(cert),
		)

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			CheckDestroy:      testAccCheckSettingsCertificateReset(cert),
			Steps: []resource.TestStep{
				{
					Config: testPritunlSettingsConfig(cert, key),
					Check:  check,
				},
				// the private key is never read back from the api
				importStep("pritunl_settings.test", "server_key"),
			},
		})
	})

	// Managing only the port must never take ownership of the certificate the
	// instance already serves: the certificate is read back into the state, so
	// it has to stay out of both the plan and the destroy.
	t.Run("manages the port without touching the certificate", func(t *testing.T) {
		if os.Getenv("TF_ACC") == "" {
			t.Skip("TF_ACC is not set, skipping the acceptance test")
		}

		settings, err := testClient.GetSettings()
		if err != nil {
			t.Fatalf("failed to read the settings of the test instance: %s", err)
		}

		certificate := strings.TrimSpace(settings.String("server_cert"))

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			CheckDestroy:      testAccCheckSettingsCertificateKept(certificate),
			Steps: []resource.TestStep{
				{
					// the port already in use, so the web server is left alone
					Config: testPritunlSettingsPortConfig(settings.Int("server_port")),
					Check: resource.ComposeTestCheckFunc(
						resource.TestCheckResourceAttr("pritunl_settings.test", "server_port", strconv.Itoa(settings.Int("server_port"))),
						// the private key is never read back, so managing only
						// the port leaves it out of the state entirely
						resource.TestCheckNoResourceAttr("pritunl_settings.test", "server_key"),
						testAccCheckSettingsCertificateKept(certificate),
					),
				},
			},
		})
	})

	// The reason the resource writes the complete settings object back: a
	// Terraform run that only manages the port must leave every other setting
	// of the instance alone. A few unrelated settings are configured out of
	// band, the way an operator would from the web console, and have to still
	// be there once Terraform has written the settings it manages.
	t.Run("leaves unrelated settings untouched", func(t *testing.T) {
		if os.Getenv("TF_ACC") == "" {
			t.Skip("TF_ACC is not set, skipping the acceptance test")
		}

		expected := configureUnrelatedSettings(t)

		settings, err := testClient.GetSettings()
		if err != nil {
			t.Fatalf("failed to read the settings of the test instance: %s", err)
		}

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			CheckDestroy:      testAccCheckUnrelatedSettings(expected),
			Steps: []resource.TestStep{
				{
					// the port already in use, so the web server is left alone
					Config: testPritunlSettingsPortConfig(settings.Int("server_port")),
					Check: resource.ComposeTestCheckFunc(
						resource.TestCheckResourceAttr("pritunl_settings.test", "id", "settings"),
						testAccCheckUnrelatedSettings(expected),
					),
				},
			},
		})
	})
}

// Settings this provider does not manage, used to prove that writing the
// managed ones hands everything else back untouched. They are deliberately not
// sso_* settings: Pritunl clears all of those on any write while single sign-on
// itself is disabled, which would fail the check for a reason that has nothing
// to do with this provider.
var unrelatedSettings = map[string]interface{}{
	"restrict_import": true,
	"email_server":    "smtp.unrelated.example.com",
	"email_from":      "unrelated@example.com",
	"pin_mode":        "optional",
}

// configureUnrelatedSettings applies the unrelated settings through a full
// object write, restores the previous values once the test is over and returns
// the values the API reports for them, so that the checks compare against what
// Pritunl actually stored rather than against what was sent.
func configureUnrelatedSettings(t *testing.T) map[string]interface{} {
	t.Helper()

	settings, err := testClient.GetSettings()
	if err != nil {
		t.Fatalf("failed to read the settings of the test instance: %s", err)
	}

	previous := make(map[string]interface{}, len(unrelatedSettings))
	for key := range unrelatedSettings {
		previous[key] = settings[key]
	}

	for key, value := range unrelatedSettings {
		settings[key] = value
	}

	if err = testClient.UpdateSettings(settings); err != nil {
		t.Fatalf("failed to configure the unrelated settings: %s", err)
	}

	t.Cleanup(func() {
		current, err := testClient.GetSettings()
		if err != nil {
			return
		}

		for key, value := range previous {
			current[key] = value
		}

		testClient.UpdateSettings(current)
	})

	settings, err = testClient.GetSettings()
	if err != nil {
		t.Fatalf("failed to read the settings of the test instance: %s", err)
	}

	expected := make(map[string]interface{}, len(unrelatedSettings))
	for key := range unrelatedSettings {
		expected[key] = settings[key]

		if fmt.Sprintf("%v", settings[key]) != fmt.Sprintf("%v", unrelatedSettings[key]) {
			t.Fatalf("the unrelated setting %q has not been configured: got %v, want %v",
				key, settings[key], unrelatedSettings[key])
		}
	}

	return expected
}

func testAccCheckUnrelatedSettings(expected map[string]interface{}) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		settings, err := testClient.GetSettings()
		if err != nil {
			return err
		}

		for key, value := range expected {
			if fmt.Sprintf("%v", settings[key]) != fmt.Sprintf("%v", value) {
				return fmt.Errorf("the unmanaged setting %q has been modified: got %v, want %v",
					key, settings[key], value)
			}
		}

		return nil
	}
}

func testPritunlSettingsPortConfig(port int) string {
	return fmt.Sprintf(`
		resource "pritunl_settings" "test" {
			server_port = %[1]d
		}
	`, port)
}

func testAccCheckSettingsCertificateKept(certificate string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		settings, err := testClient.GetSettings()
		if err != nil {
			return err
		}

		if strings.TrimSpace(settings.String("server_cert")) != certificate {
			return fmt.Errorf("the certificate of the pritunl instance has been modified")
		}

		return nil
	}
}

func testPritunlSettingsConfig(cert, key string) string {
	return fmt.Sprintf(`
		resource "pritunl_settings" "test" {
			server_cert = %[1]q
			server_key  = %[2]q
		}
	`, cert, key)
}

func testAccCheckSettingsCertificate(cert string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		settings, err := testClient.GetSettings()
		if err != nil {
			return err
		}

		if strings.TrimSpace(settings.String("server_cert")) != cert {
			return fmt.Errorf("the certificate has not been applied on the pritunl instance")
		}

		return nil
	}
}

// Destroying the resource must leave the instance with a working certificate of
// its own, otherwise the following tests would not be able to reach the api.
func testAccCheckSettingsCertificateReset(cert string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		settings, err := testClient.GetSettings()
		if err != nil {
			return err
		}

		if strings.TrimSpace(settings.String("server_cert")) == "" {
			return fmt.Errorf("the pritunl instance has been left without a certificate")
		}

		if strings.TrimSpace(settings.String("server_cert")) == cert {
			return fmt.Errorf("the certificate is still applied on the pritunl instance")
		}

		return nil
	}
}

// The returned pair is trimmed to match what Pritunl stores and what the
// provider keeps in the state.
func generateSelfSignedCertificate(t *testing.T) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate the test private key: %s", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   "pritunl.local",
			Organization: []string{"terraform-provider-pritunl"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"pritunl.local", "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certificate, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to generate the test certificate: %s", err)
	}

	certPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return strings.TrimSpace(string(certPem)), strings.TrimSpace(string(keyPem))
}
