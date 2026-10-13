#!/bin/bash
# Executado pelo entrypoint do PostgreSQL apenas na primeira inicialização do
# volume. Cria os dois papéis da aplicação e o banco:
#   wallet_owner  dono do schema; usado só pelas migrations (CREATEDB permite
#                 que os testes de integração criem bancos temporários)
#   wallet_app    papel da aplicação; recebe apenas os privilégios concedidos
#                 pelas migrations (sem UPDATE/DELETE no ledger)
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_password="$WALLET_OWNER_PASSWORD" \
  -v app_password="$WALLET_APP_PASSWORD" \
  -v database="$WALLET_DB" <<'SQL'
CREATE ROLE wallet_owner LOGIN CREATEDB PASSWORD :'owner_password';
CREATE ROLE wallet_app LOGIN PASSWORD :'app_password';
CREATE DATABASE :"database" OWNER wallet_owner;
SQL
