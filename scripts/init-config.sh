#!/bin/sh
# Generate credentials once; never silently rotate an initialized PostgreSQL password.
# The private YAML files are bootstrap inputs. Runtime settings are persisted by services.
set -eu
umask 077

template_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$template_root"
generated_count=0
for config_path in .local/postgres_password configs/config.local.yaml configs/config.docker.yaml .local/compose.env; do
    if [ -e "$config_path" ]; then generated_count=$((generated_count + 1)); fi
done
if [ "$generated_count" -eq 4 ]; then
    printf '%s\n' 'Private configuration already exists; no files changed.'
    exit 0
fi
if [ "$generated_count" -ne 0 ]; then
    printf '%s\n' 'Private configuration is incomplete; resolve existing files manually. Nothing was overwritten.' >&2
    exit 1
fi
command -v openssl >/dev/null 2>&1 || { printf '%s\n' 'openssl is required to generate cryptographic credentials.' >&2; exit 1; }

database_password=$(openssl rand -hex 32)
admin_password=$(openssl rand -hex 24)
encryption_key=$(openssl rand -base64 32)
config_group=$(id -g)
mkdir -p .local
chmod 700 .local
printf '%s\n' "$database_password" > .local/postgres_password
# Base64 uses no pipe character, so it cannot change these sed replacement expressions.
sed -e "s|REPLACE_WITH_DATABASE_PASSWORD|$database_password|" \
    -e "s|REPLACE_WITH_ADMIN_PASSWORD|$admin_password|" \
    -e "s|REPLACE_WITH_BASE64_ENCRYPTION_KEY|$encryption_key|" \
    -e 's|  port: 5432|  port: 55434|' \
    -e 's|  addr: ":8080"|  addr: ":18081"|' \
    configs/config.example.yaml > configs/config.local.yaml
sed -e "s|REPLACE_WITH_DATABASE_PASSWORD|$database_password|" \
    -e "s|REPLACE_WITH_ADMIN_PASSWORD|$admin_password|" \
    -e "s|REPLACE_WITH_BASE64_ENCRYPTION_KEY|$encryption_key|" \
    -e 's|  host: 127.0.0.1|  host: postgres|' \
    configs/config.example.yaml > configs/config.docker.yaml
# Only the config's group ID is an environment value; passwords remain in private files.
printf 'HEAGO_CONFIG_GID=%s\n' "$config_group" > .local/compose.env
chmod 600 .local/postgres_password configs/config.local.yaml .local/compose.env
chgrp "$config_group" configs/config.docker.yaml
chmod 640 configs/config.docker.yaml
printf '%s\n' 'Created private PostgreSQL/bootstrap configuration; credentials were not printed.'
printf '%s\n' 'Docker: docker compose --env-file .local/compose.env up -d --build app'
