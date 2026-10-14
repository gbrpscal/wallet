#!/bin/bash
# Testes de mutação curados: cada mutante injeta um defeito em uma cópia
# descartável do código e roda os testes. O mutante precisa ser "morto" (algum
# teste falhar). Mutante que sobrevive indica teste que não verifica a regra.
#
# Uso (pelo container de toolchain):
#   docker compose run --rm tools bash scripts/mutation.sh
#
# Os mutantes de persistência rodam os testes com a tag integration e exigem
# o PostgreSQL do Compose no ar (docker compose up -d --wait postgres). Antes
# de cada grupo, os testes rodam sem mutação: se essa linha de base falhar, o
# script aborta, para que uma falha de ambiente não conte como mutante morto.
#
# Resultados:
#   KILLED   um teste falhou por causa do defeito (esperado)
#   SURVIVED nenhum teste falhou: a regra não está protegida por teste
#   INVALID  o mutante não compilou; não prova nada e deve ser corrigido
#   ERROR    o trecho a mutar não foi encontrado (o código mudou)
#
# O script termina com código 1 se houver qualquer resultado diferente de KILLED.
set -u

SRC="${SRC:-$(cd "$(dirname "$0")/.." && pwd)}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

killed=0
bad=0

# Um mutante que trave o teste (por exemplo, esperando um lock para sempre) é
# morto pelo timeout.
TEST_TIMEOUT="${TEST_TIMEOUT:-120s}"

# baseline <pacotes> [tags]: os testes precisam passar sem mutação.
baseline() {
  local pkgs="$1" tags="${2:-}"
  if ! (cd "$SRC" && go test -count=1 -timeout "$TEST_TIMEOUT" ${tags:+-tags=$tags} $pkgs >/dev/null 2>&1); then
    echo "ERROR    linha de base falhou para $pkgs ${tags:+(tags: $tags)}; corrija o ambiente antes"
    exit 1
  fi
}

# mutate <nome> <arquivo> <trecho original> <trecho mutado> [pacotes] [tags]
mutate() {
  local name="$1" file="$2" from="$3" to="$4" pkgs="${5:-./internal/domain/...}" tags="${6:-}"
  rm -rf "$WORK/src" && cp -r "$SRC" "$WORK/src"
  if ! FROM="$from" TO="$to" perl -0pi -e 's/\Q$ENV{FROM}\E/$ENV{TO}/ or exit 3' "$WORK/src/$file"; then
    echo "ERROR    $name (trecho não encontrado em $file)"
    bad=$((bad + 1))
    return
  fi
  local out status
  out=$(cd "$WORK/src" && go test -count=1 -timeout "$TEST_TIMEOUT" ${tags:+-tags=$tags} $pkgs 2>&1)
  status=$?
  if echo "$out" | grep -qE '\[build failed\]|\[setup failed\]'; then
    echo "INVALID  $name (o mutante não compila)"
    bad=$((bad + 1))
  elif [ $status -ne 0 ]; then
    local tests
    tests=$(echo "$out" | grep -E '^\s*--- FAIL' | sed -E 's/^\s*--- FAIL: ([^ ]+).*/\1/' | head -3 | paste -sd, -)
    if [ -z "$tests" ] && echo "$out" | grep -q 'test timed out'; then
      tests="timeout: $(echo "$out" | grep -A1 'running tests:' | tail -1 | xargs)"
    fi
    echo "KILLED   $name <- $tests"
    killed=$((killed + 1))
  else
    echo "SURVIVED $name"
    bad=$((bad + 1))
  fi
}

baseline ./internal/domain/...

# --- Money ------------------------------------------------------------------
mutate "aceita zeros à esquerda (01.00)" internal/domain/money/money.go \
  'if len(intPart) > 1 && intPart[0] ==' 'if false && intPart[0] =='
mutate "soma sem checar overflow" internal/domain/money/money.go \
  'if (o.minor > 0 && m.minor > math.MaxInt64-o.minor)' 'if false && (o.minor > 0 && m.minor > math.MaxInt64-o.minor)'
mutate "negação sem checar overflow" internal/domain/money/money.go \
  'if m.minor == math.MinInt64 {' 'if false {'
mutate "aritmética aceita moedas diferentes" internal/domain/money/money.go \
  'if m.currency != o.currency {' 'if false {'
mutate "parsing aceita três casas decimais" internal/domain/money/money.go \
  'dot := len(s) - Scale - 1' 'dot := len(s) - Scale - 2'

# --- Wallet e ledger ---------------------------------------------------------
mutate "débito deixa o saldo negativo" internal/domain/wallet/wallet.go \
  'if cmp < 0 {' 'if cmp < -1 {'
mutate "versão não incrementa" internal/domain/wallet/wallet.go \
  'w.version = entry.WalletVersion()' ''
mutate "lançamento grava a versão anterior" internal/domain/wallet/wallet.go \
  'w.balance, after, w.version+1, now)' 'w.balance, after, w.version, now)'
mutate "ledger aceita balanceAfter errado" internal/domain/wallet/ledger.go \
  'if !expected.Equal(s.BalanceAfter) {' 'if !expected.Equal(s.BalanceAfter) && false {'
mutate "abertura com saldo zero cria lançamento" internal/domain/wallet/wallet.go \
  'if initial.IsZero() {
		return w, nil, nil
	}' ''
mutate "reidratação aceita saldo negativo" internal/domain/wallet/wallet.go \
  'case s.Balance.IsNegative():' 'case false:'

# --- WagerTransaction e regras ----------------------------------------------
mutate "estado terminal volta a transitar" internal/domain/wagering/transaction.go \
  'PendingReference: {Processed, Rejected, Failed},' 'PendingReference: {Processed, Rejected, Failed}, Processed: {Rejected},'
mutate "OPENING aceito via HTTP/SQS" internal/domain/wagering/transaction.go \
  'case Bet, Win, Loss, Refund, Rollback:
		return k, nil
	default:
		return "", domain.Errorf(domain.ErrKindNotAllowed' 'case Bet, Win, Loss, Refund, Rollback, Opening:
		return k, nil
	default:
		return "", domain.Errorf(domain.ErrKindNotAllowed'
mutate "replay ignora payload diferente" internal/domain/wagering/transaction.go \
  'if t.payloadHash != req.hash {' 'if false {'
mutate "replay ignora chave diferente" internal/domain/wagering/transaction.go \
  'if t.idempotencyKey != req.idempotencyKey {' 'if false {'
mutate "LOSS aceita valor diferente de zero" internal/domain/wagering/request.go \
  'if kind == Loss && !m.IsZero() {' 'if false {'
mutate "BET aceita valor zero" internal/domain/wagering/request.go \
  'if kind != Loss && !m.IsPositive() {' 'if false {'
mutate "LOSS movimenta saldo" internal/domain/wagering/process.go \
  'return p.processed(nil, domain.TransactionID{})' 'return p.move(wallet.Credit, domain.TransactionID{}, "")'
mutate "segunda reversão aceita" internal/domain/wagering/process.go \
  'if ref.Reversed {' 'if false {'
mutate "reversão aceita valor diferente" internal/domain/wagering/process.go \
  'if !r.money.Equal(p.tx.money) {' 'if false {'
mutate "reversão aceita outra rodada" internal/domain/wagering/process.go \
  'r.roundID == p.tx.roundID' 'true'
mutate "REFUND aceita referência a WIN" internal/domain/wagering/process.go \
  'case Refund:
		return target == Bet' 'case Refund:
		return target == Bet || target == Win'
mutate "rollback de WIN credita em vez de debitar" internal/domain/wagering/process.go \
  'if r.kind == Bet {' 'if true {'
mutate "reversão sem saldo usa o código da aposta" internal/domain/wagering/process.go \
  'return p.move(wallet.Debit, r.id, domain.CodeReversalInsufficient)' 'return p.move(wallet.Debit, r.id, domain.CodeInsufficientFunds)'
mutate "reversão com referência rejeitada fica esperando" internal/domain/wagering/process.go \
  'case Rejected, Failed:
		return p.reject(domain.CodeReferenceNotProcessed)' 'case Rejected, Failed:
		return p.wait()'

# --- Hash de idempotência ------------------------------------------------------
mutate "hash ignora walletId" internal/domain/wagering/request.go \
  '"walletId":              r.walletID.String(),' ''
mutate "hash inclui a idempotency key" internal/domain/wagering/request.go \
  '"gameId":                r.gameID,' '"gameId": r.gameID, "key": r.idempotencyKey,'
mutate "hash com escape de HTML" internal/domain/wagering/request.go \
  'enc.SetEscapeHTML(false)' 'enc.SetEscapeHTML(true)'

# --- Persistência (tag integration; exige o PostgreSQL no ar) --------------------
PG=./internal/adapters/postgres/...
baseline $PG integration

mutate "banco aceita saldo negativo" migrations/00001_create_wallets.sql \
  'balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0),' 'balance_minor BIGINT NOT NULL,' $PG integration
mutate "banco aceita versão sem mudança de saldo" migrations/00001_create_wallets.sql \
  'ELSIF NEW.version <> OLD.version THEN' 'ELSIF false THEN' $PG integration
mutate "aplicação pode alterar e apagar o ledger" migrations/00003_create_wallet_ledger_entries.sql \
  'GRANT SELECT, INSERT ON wallet_ledger_entries TO wallet_app;' 'GRANT SELECT, INSERT, UPDATE, DELETE ON wallet_ledger_entries TO wallet_app;' $PG integration
mutate "dono do schema pode apagar lançamento" migrations/00003_create_wallet_ledger_entries.sql \
  'CREATE TRIGGER wallet_ledger_entries_no_delete BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_immutable();' '' $PG integration
mutate "saldo confirmado sem lançamento" migrations/00003_create_wallet_ledger_entries.sql \
  'CREATE CONSTRAINT TRIGGER wallets_ledger_consistency
    AFTER INSERT OR UPDATE ON wallets' 'CREATE CONSTRAINT TRIGGER wallets_ledger_consistency
    AFTER INSERT ON wallets' $PG integration
mutate "cadeia do ledger não conferida" migrations/00003_create_wallet_ledger_entries.sql \
  'IF NEW.wallet_version <> prev.wallet_version + 1
            OR NEW.balance_before_minor <> prev.balance_after_minor THEN' 'IF false THEN' $PG integration
mutate "duas reversões da mesma aposta" migrations/00002_create_wager_transactions.sql \
  'CREATE UNIQUE INDEX wager_transactions_one_reversal_uq' 'CREATE INDEX wager_transactions_one_reversal_uq' $PG integration
mutate "crédito inicial duplicado" migrations/00002_create_wager_transactions.sql \
  'CREATE UNIQUE INDEX wager_transactions_opening_uq' 'CREATE INDEX wager_transactions_opening_uq' $PG integration
mutate "chave de idempotência global (sem isolamento por provedor)" migrations/00002_create_wager_transactions.sql \
  'ON wager_transactions (provider_id, idempotency_key) WHERE' 'ON wager_transactions (idempotency_key) WHERE' $PG integration
mutate "transação volta para PENDING" migrations/00002_create_wager_transactions.sql \
  "AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))" "AND NEW.status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))" $PG integration
mutate "banco aceita código de falha desconhecido" migrations/00002_create_wager_transactions.sql \
  "OR (status = 'REJECTED' AND failure_code IN (" "OR (status = 'REJECTED' AND failure_code IS NOT NULL) OR (false AND failure_code IN (" $PG integration
mutate "payload da outbox pode mudar" migrations/00004_create_inbox_outbox.sql \
  'NEW.payload::text, NEW.occurred_at)' 'NEW.payload::text, NEW.occurred_at, NULL)' $PG integration
mutate "lock da carteira sem FOR UPDATE" internal/adapters/postgres/wallets.go \
  'FROM wallets WHERE id = $1 FOR UPDATE`' 'FROM wallets WHERE id = $1`' $PG integration
mutate "UPDATE sem checar a versão" internal/adapters/postgres/wallets.go \
  'WHERE id = $1 AND version = $5`' 'WHERE id = $1 AND $5::bigint IS NOT NULL`' $PG integration
mutate "lock_timeout não classificado como transitório" internal/adapters/postgres/errors.go \
  '"55P03": true,' '' $PG integration
mutate "lock_timeout não aplicado" internal/adapters/postgres/uow.go \
  'if u.lockTimeout > 0 {' 'if false {' $PG integration
mutate "busca de replay sem escopo de provedor" internal/adapters/postgres/transactions.go \
  'WHERE provider_id = $1 AND idempotency_key = $2' 'WHERE $1::text IS NOT NULL AND idempotency_key = $2' $PG integration

# --- Casos de uso (tag integration; exige o PostgreSQL no ar) --------------------
APP=./internal/app/...
baseline $APP integration

mutate "replay não confere o payload" internal/app/service.go \
  'if err := existing.CheckReplay(req); err != nil {' 'if false {' $APP integration
mutate "carteira inexistente sem WALLET_NOT_FOUND" internal/app/service.go \
  'return OperationResult{}, domain.Errorf(domain.ErrWalletNotFound, "wallet %s not found", req.WalletID())' 'return OperationResult{}, err' $APP integration
mutate "pendência gravada sem agendamento" internal/app/service.go \
  'meta.Schedule = &sched' '_ = sched' $APP integration
mutate "worker ignora o horário agendado" internal/app/service.go \
  '|| now.Before(info.Schedule.NextAttemptAt) {' '{' $APP integration
mutate "reversão anterior ignorada" internal/app/service.go \
  'return wagering.Reference{Transaction: r, Reversed: reversed}, nil' 'return wagering.Reference{Transaction: r, Reversed: false && reversed}, nil' $APP integration
mutate "reconciliação sempre consistente" internal/app/service.go \
  'Consistent: diff.IsZero(),' 'Consistent: true,' $APP integration
mutate "cursor repete o último lançamento" internal/app/service.go \
  'EncodeCursor(entries[limit-1].WalletVersion())' 'EncodeCursor(entries[limit-1].WalletVersion() - 1)' $APP integration
mutate "espera não respeita o máximo de tentativas" internal/app/policy.go \
  'return s.Attempts >= p.MaxAttempts ||' 'return s.Attempts > p.MaxAttempts ||' $APP integration
mutate "espera ignora o TTL" internal/app/policy.go \
  '|| !now.Before(s.ExpiresAt)' '|| false' $APP integration
mutate "backoff sem teto" internal/app/policy.go \
  'if d > p.MaxDelay {
		d = p.MaxDelay
	}' '' $APP
mutate "retry não repete falha transitória" internal/app/policy.go \
  'return IsRetryable(err) || errors.Is(err, ErrDuplicate)' 'return errors.Is(err, ErrDuplicate)' $APP
mutate "cursor aceita versão negativa" internal/app/policy.go \
  '|| c.V != 1 || c.After < 0 {' '|| c.V != 1 {' $APP

# --- HTTP e autenticação (tag integration; exige PostgreSQL e Keycloak no ar) ---------
API=./internal/adapters/httpapi/...
baseline $API integration

mutate "provedor opera em nome de outro" internal/adapters/httpapi/handlers.go \
  'if p.ProviderID == "" || (req.ProviderID != "" && req.ProviderID != p.ProviderID) {' 'if p.ProviderID == "" {' $API integration
mutate "provedor lê transação de outro" internal/adapters/httpapi/handlers.go \
  'if err == nil && (t.IsInternal() || t.ProviderID() != principalFrom(r.Context()).ProviderID) {' 'if err == nil && t.IsInternal() {' $API integration
mutate "rota por id externo sem escopo de provedor" internal/adapters/httpapi/handlers.go \
  'if providerID != principalFrom(r.Context()).ProviderID {' 'if false {' $API integration
mutate "scope não conferido" internal/adapters/httpapi/auth.go \
  'if !p.Has(scope) {' 'if false {' $API integration
mutate "audiência não conferida" internal/adapters/httpapi/auth.go \
  'ClientID:             cfg.OIDCAudience,' 'SkipClientIDCheck: true,' $API integration
mutate "expiração não conferida" internal/adapters/httpapi/auth.go \
  'SupportedSigningAlgs: []string{oidc.RS256},' 'SupportedSigningAlgs: []string{oidc.RS256}, SkipExpiryCheck: true,' $API integration
mutate "assinatura não conferida" internal/adapters/httpapi/auth.go \
  'token, err := a.verifier.Verify(ctx, raw)' 'token, err := oidc.NewVerifier("", nil, &oidc.Config{InsecureSkipSignatureCheck: true, SkipIssuerCheck: true, SkipClientIDCheck: true}).Verify(ctx, raw)' $API integration
mutate "campos desconhecidos aceitos" internal/adapters/httpapi/problem.go \
  'dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {' 'if err := dec.Decode(dst); err != nil {' $API integration
mutate "rejeição respondida como sucesso" internal/adapters/httpapi/handlers.go \
  'case wagering.Rejected:
		return http.StatusUnprocessableEntity' 'case wagering.Rejected:
		return http.StatusOK' $API integration
mutate "Idempotency-Key opcional" internal/adapters/httpapi/handlers.go \
  'if key == "" {' 'if false {' $API integration
mutate "X-Correlation-Id do cliente ignorado" internal/adapters/httpapi/correlation.go \
  'if domain.ValidateTextID("correlationId", id) != nil {' 'if true {' $API integration

echo
echo "mutantes mortos: $killed, problemas: $bad"
[ "$bad" -eq 0 ]
