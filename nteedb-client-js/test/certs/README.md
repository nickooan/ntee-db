# Test-only TLS certificate

`server.crt` / `server.key` are a **test-only** self-signed pair (CN
`nteedb-client-js-test`, SANs `localhost`/`127.0.0.1`/`::1`) used by the TLS
test suite to start `nteedb-server` with `-tls-cert`/`-tls-key`. The private
key is deliberately committed — it protects nothing. Never use these outside
tests. Regenerate with:

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout server.key -out server.crt -days 36500 \
  -subj "/CN=nteedb-client-js-test" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1"
```
