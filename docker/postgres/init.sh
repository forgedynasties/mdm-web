#!/bin/bash
# Runs on every container start. Ensures the mdm user password always
# matches POSTGRES_PASSWORD, even if the volume predates this .env.
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    ALTER USER $POSTGRES_USER WITH PASSWORD '$POSTGRES_PASSWORD';
EOSQL
