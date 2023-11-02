#!/bin/sh

if test -n "$DATABASE_CLIENT_CERT_B64"; then
  echo "$DATABASE_CLIENT_CERT_B64" | base64 -d >/tmp/client-cert.pem
fi

if test -n "$DATABASE_CLIENT_KEY_B64"; then
  echo "$DATABASE_CLIENT_KEY_B64" | base64 -d >/tmp/client-key.pem
  chmod 0600 /tmp/client-key.pem
fi

if test -n "$DATABASE_SERVER_CA_B64"; then
  echo "$DATABASE_SERVER_CA_B64" | base64 -d >/tmp/ca.pem
fi

exec /migrate "$@"
