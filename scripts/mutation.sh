#!/bin/bash
# Testes de mutação curados: cada mutante injeta um defeito em uma cópia
# descartável do código e roda os testes. O mutante precisa ser "morto" (algum
# teste falhar). Mutante que sobrevive indica teste que não verifica a regra.
#
# Uso (pelo container de toolchain):
#   docker compose run --rm tools bash scripts/mutation.sh
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
  out=$(cd "$WORK/src" && go test -count=1 ${tags:+-tags=$tags} $pkgs 2>&1)
  status=$?
  if echo "$out" | grep -qE '\[build failed\]|\[setup failed\]'; then
    echo "INVALID  $name (o mutante não compila)"
    bad=$((bad + 1))
  elif [ $status -ne 0 ]; then
    local tests
    tests=$(echo "$out" | grep -E '^\s*--- FAIL' | sed -E 's/^\s*--- FAIL: ([^ ]+).*/\1/' | head -3 | paste -sd, -)
    echo "KILLED   $name <- $tests"
    killed=$((killed + 1))
  else
    echo "SURVIVED $name"
    bad=$((bad + 1))
  fi
}

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
  'w.version++' ''
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

echo
echo "mutantes mortos: $killed, problemas: $bad"
[ "$bad" -eq 0 ]
