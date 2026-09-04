# The resource always writes the complete settings object back: it reads the
# settings of the instance immediately before each write and overlays only the
# attributes below on top of them, the same way the Pritunl web console does.
# `PUT /settings` is a full replace and not a partial update, so applying this
# resource leaves the single sign-on, SMTP, monitoring and every other unmanaged
# setting of the instance untouched.

# Rotating the TLS certificate served by the Pritunl web console and API from a
# certificate stored in Azure Key Vault.
#
# This is the recommended way to source the pair: the azurerm provider parses
# the PKCS12 bundle natively and exposes plain PEM, so `pem` and `key` are
# passed straight through. Pritunl expects raw PEM strings, so unlike the
# Imperva/Incapsula modules they must not be base64 encoded.

data "azurerm_key_vault" "example" {
  name                = "examplekv"
  resource_group_name = "some-resource-group"
}

data "azurerm_key_vault_certificate_data" "example" {
  name         = "my-cert-name"
  key_vault_id = data.azurerm_key_vault.example.id
}

resource "pritunl_settings" "main" {
  server_cert = data.azurerm_key_vault_certificate_data.example.pem
  server_key  = data.azurerm_key_vault_certificate_data.example.key
}

# `pem` holds every certificate of the bundle, `certificates_count` tells how
# many. Should the bundle also carry the root CA, keep the leaf and the
# intermediate(s) only and drop it, clients already trust it through their own
# trust store:
#
#   locals {
#     certificate_blocks = regexall(
#       "-----BEGIN CERTIFICATE-----[^-]+-----END CERTIFICATE-----",
#       data.azurerm_key_vault_certificate_data.example.pem,
#     )
#
#     server_cert = join("\n", slice(local.certificate_blocks, 0, 2))
#   }

# Building the value from a PKCS12 bundle any other way, for instance from a
# local file or an `external` data source shelling out to
# `openssl pkcs12 -nokeys -legacy`, needs that same clean up: the OpenSSL CLI
# writes "Bag Attributes", "subject=" and "issuer=" lines in front of every
# certificate, which are not part of the PEM format and which Pritunl rejects.
# The regexall above removes them, and so does this pipeline:
#
#   openssl pkcs12 -in vpn.pfx -nokeys -legacy |
#     sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p' |
#     awk '/BEGIN CERTIFICATE/ { n++ } n <= 2' > server_cert.pem

# Managing only the port, on an instance whose certificate is handled elsewhere.
# The certificate already installed is read back and handed over untouched, so
# this never takes ownership of it:
#
#   resource "pritunl_settings" "port_only" {
#     server_port = 8443
#   }
#
# Changing the port makes Pritunl restart its web server on the new one, so the
# `url` of the provider has to be updated before the next run.
