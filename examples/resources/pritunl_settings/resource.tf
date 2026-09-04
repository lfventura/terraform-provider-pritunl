# Rotating the TLS certificate served by the Pritunl web console and API from a
# PKCS12 bundle kept in Azure Key Vault.

data "azurerm_key_vault" "shared" {
  name                = "yapebo-shared-kv"
  resource_group_name = "yapebo-shared-rg"
}

data "azurerm_key_vault_certificate_data" "vpn" {
  name         = "vpn-yapebo-com"
  key_vault_id = data.azurerm_key_vault.shared.id
}

locals {
  # Keep the certificate blocks and nothing else. PKCS12 tooling such as
  # `openssl pkcs12 -nokeys -legacy` writes OpenSSL specific "Bag Attributes",
  # "subject=" and "issuer=" lines in front of every block. They are not part of
  # the PEM format and Pritunl rejects a certificate that still carries them.
  certificate_blocks = regexall(
    "-----BEGIN CERTIFICATE-----[^-]+-----END CERTIFICATE-----",
    data.azurerm_key_vault_certificate_data.vpn.pem,
  )

  # Pritunl expects the leaf certificate followed by the intermediate(s), in
  # that order. The root CA is deliberately left out: clients already trust it
  # through their own trust store and only need the intermediates to build the
  # chain.
  server_cert = join("\n", slice(local.certificate_blocks, 0, 2))
}

resource "pritunl_settings" "this" {
  server_cert = local.server_cert
  server_key  = data.azurerm_key_vault_certificate_data.vpn.key
}

# The same normalisation outside of Terraform, for a bundle exported by hand:
#
#   openssl pkcs12 -in vpn.pfx -nokeys -legacy |
#     sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p' |
#     awk '/BEGIN CERTIFICATE/ { n++ } n <= 2' > server_cert.pem
#
# The sed drops the "Bag Attributes", "subject=" and "issuer=" lines, the awk
# keeps the leaf and the intermediate and discards the root CA.
