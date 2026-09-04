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

		certificate := ""
		if settings.ServerCert != nil {
			certificate = strings.TrimSpace(*settings.ServerCert)
		}

		resource.Test(t, resource.TestCase{
			PreCheck:          func() { preCheck(t) },
			ProviderFactories: providerFactories,
			CheckDestroy:      testAccCheckSettingsCertificateKept(certificate),
			Steps: []resource.TestStep{
				{
					// the port already in use, so the web server is left alone
					Config: testPritunlSettingsPortConfig(settings.ServerPort),
					Check: resource.ComposeTestCheckFunc(
						resource.TestCheckResourceAttr("pritunl_settings.test", "server_port", strconv.Itoa(settings.ServerPort)),
						// the private key is never read back, so managing only
						// the port leaves it out of the state entirely
						resource.TestCheckNoResourceAttr("pritunl_settings.test", "server_key"),
						testAccCheckSettingsCertificateKept(certificate),
					),
				},
			},
		})
	})
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

		if settings.ServerCert == nil || strings.TrimSpace(*settings.ServerCert) != certificate {
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

		if settings.ServerCert == nil || strings.TrimSpace(*settings.ServerCert) != cert {
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

		if settings.ServerCert == nil || strings.TrimSpace(*settings.ServerCert) == "" {
			return fmt.Errorf("the pritunl instance has been left without a certificate")
		}

		if strings.TrimSpace(*settings.ServerCert) == cert {
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
