#!/bin/sh

if test -n "$CLIENT_CERT_B64"; then
  echo "$CLIENT_CERT_B64" | base64 -d >/tmp/client-cert.pem
fi

if test -n "$CLIENT_KEY_B64"; then
  echo "$CLIENT_KEY_B64" | base64 -d >/tmp/client-key.pem
fi

if test -n "$CA_B64"; then
  echo "$CA_B64" | base64 -d >/tmp/ca.pem
fi

exec /migrate "$@"
