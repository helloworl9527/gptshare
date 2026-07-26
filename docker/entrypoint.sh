#!/bin/sh
set -eu

install -d -o vitals -g vitals -m 0700 /data
chown -R vitals:vitals /data

su-exec vitals:vitals vitals-migrate --validate-only
su-exec vitals:vitals vitals-migrate
exec su-exec vitals:vitals vitals
