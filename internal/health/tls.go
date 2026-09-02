package health

import "crypto/tls"

// tlsConfigInsecure implements the v1 upstream-TLS choice: certificates of
// https servers are not verified. Self-signed certificates are the norm on
// homelab and VLAN inference boxes; the honest trade is stated in the
// functional spec — anyone on the path can read and modify that traffic and
// sees the forwarded auth_token. Verification and pinning are deferred.
// #nosec G402 -- deliberate and documented, not a mistake to silence.
var tlsConfigInsecure = tls.Config{InsecureSkipVerify: true}
