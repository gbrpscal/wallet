# Wallet

Serviço em Go de carteiras de jogadores e wagering. Expõe uma API HTTP e um consumidor SQS FIFO que movimentam saldo com as mesmas garantias de integridade financeira, concorrência e idempotência.

As decisões técnicas estão em [ARCHITECTURE.md](ARCHITECTURE.md).

> **Em desenvolvimento.** Este README é atualizado a cada fase. Os itens marcados como *pendente* ainda não estão disponíveis.

## Sumário

- [Pré-requisitos](#pré-requisitos)
- [Container de toolchain](#container-de-toolchain)
- [Execução da aplicação](#execução-da-aplicação)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Filas SQS](#filas-sqs)
- [Banco de dados e migrations](#banco-de-dados-e-migrations)
- [Exemplos de chamadas](#exemplos-de-chamadas)
- [Testes](#testes)
- [Testes de integração, múltiplas instâncias e falha](#testes-de-integração-múltiplas-instâncias-e-falha)
- [Estrutura do repositório](#estrutura-do-repositório)

## Pré-requisitos

- **Docker com Compose** (testado com Docker 29 e Compose v5). É o único pré-requisito: Go, ferramentas de migrations e demais utilitários rodam em containers.
- Opcional: Go 1.27.2, para quem preferir os comandos nativos.

## Container de toolchain

O serviço `tools` do Compose traz Go 1.27.2 (com CGO, para `-race`) e `goose`. O código é montado como volume, e os caches de módulos e de build ficam em volumes nomeados, então não são baixados a cada execução. Ele só sobe quando chamado explicitamente.

| Ação | Containerizado (padrão) | Nativo |
|---|---|---|
| Compilar | `docker compose run --rm tools go build ./...` | `go build ./...` |
| Testes unitários | `docker compose run --rm tools go test ./...` | `go test ./...` |
| Testes com race detector | `docker compose run --rm tools go test -race ./...` | `go test -race ./...` |
| Análise estática | `docker compose run --rm tools go vet ./...` | `go vet ./...` |
| Formatação (lista arquivos fora do padrão) | `docker compose run --rm tools gofmt -l .` | `gofmt -l .` |
| Testes de mutação | `docker compose run --rm tools bash scripts/mutation.sh` | `bash scripts/mutation.sh` (requer `perl`) |

O container roda com o UID/GID `1000:1000`, para que os arquivos gerados (como `go.sum`) pertençam ao seu usuário. Se o seu UID for outro, crie um `.env` com os valores de `id -u` e `id -g`:

```bash
cp .env.example .env
```

e ajuste `HOST_UID` e `HOST_GID`.

## Execução da aplicação

Sobe PostgreSQL, Keycloak, LocalStack, Toxiproxy e a aplicação:

```bash
docker compose up --build
```

Em segundo plano, aguardando os health checks:

```bash
docker compose up --build -d --wait
```

Para encerrar (o comando `down -v` também remove o volume do banco):

```bash
docker compose down
```

| Serviço | Endereço no host | Endereço na rede do Compose |
|---|---|---|
| API | `http://localhost:8080` | `http://app:8080` |
| Keycloak | `http://localhost:8180` | `http://keycloak:8080` |
| PostgreSQL | não publicado | `postgres:5432` |
| LocalStack (SQS) | não publicado | `http://localstack:4566` |
| Toxiproxy | não publicado | `toxiproxy:8474` (API), `:15432` (PostgreSQL), `:14566` (SQS) |

### LocalStack em outra máquina

O projeto fixa `localstack/localstack:4.14.0`, a última linha comunitária que sobe **sem conta**. As imagens mais novas (`latest`) encerram com "License activation failed" se não houver token. Para usar uma versão com licença:

1. Troque a tag do serviço `localstack` no `compose.yaml`.
2. Defina `LOCALSTACK_AUTH_TOKEN` no ambiente desse serviço.

## Variáveis de ambiente

Os valores padrão estão no `compose.yaml`, e o projeto funciona sem `.env`. O `.env.example` lista os valores que podem ser sobrescritos; ele não contém segredos reais.

**Aplicação**

| Variável | Padrão | Descrição |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Endereço do servidor HTTP |
| `DATABASE_URL` | *(obrigatória)* | URL de conexão do PostgreSQL, com o papel `wallet_app` |
| `DB_LOCK_TIMEOUT` | `5s` | Espera máxima por lock de linha; ao estourar, a operação falha como transitória e nada é confirmado |
| `REFERENCE_MAX_ATTEMPTS` | `10` | Tentativas do worker para uma reversão que espera a transação referenciada |
| `REFERENCE_TTL` | `10m` | Prazo máximo de espera, contado da criação; o que vier primeiro (tentativas ou prazo) encerra com `REFERENCE_NOT_FOUND` |
| `REFERENCE_BACKOFF_BASE` / `REFERENCE_BACKOFF_MAX` | `1s` / `60s` | Backoff exponencial entre tentativas (com ±20% de jitter) |
| `OIDC_ISSUER` | *(obrigatória)* | Emissor esperado nos tokens (`http://localhost:8180/realms/wallet` no Compose) |
| `OIDC_JWKS_URL` | *(obrigatória)* | Chaves públicas do Keycloak (`http://keycloak:8080/.../certs` no Compose) |
| `OIDC_AUDIENCE` | `wallet-api` | Audiência exigida nos tokens |
| `AWS_REGION` | `us-east-1` | Região do SQS |
| `AWS_ENDPOINT_URL` | — | Endpoint do SQS (LocalStack) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `test` / `test` | Credenciais (lidas pelo SDK da AWS) |
| `SQS_WAGER_QUEUE_NAME` | `wager-transactions.fifo` | Fila de entrada |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |
| `SHUTDOWN_TIMEOUT` | `30s` | Prazo para concluir o trabalho em andamento no encerramento |
| `READINESS_TIMEOUT` | `2s` | Timeout de cada checagem de readiness |
| `HTTP_READ_TIMEOUT` / `HTTP_WRITE_TIMEOUT` / `HTTP_READ_HEADER_TIMEOUT` | `10s` / `15s` / `5s` | Timeouts do servidor HTTP |

**Compose (`.env`)**

| Variável | Padrão | Descrição |
|---|---|---|
| `HOST_UID` / `HOST_GID` | `1000` | Usuário do container `tools` |
| `APP_HOST_PORT` / `KEYCLOAK_HOST_PORT` | `8080` / `8180` | Portas publicadas no host |
| `POSTGRES_PASSWORD` | `postgres` | Senha do superusuário, usado só na criação dos papéis |
| `WALLET_DB` | `wallet` | Nome do banco da aplicação |
| `WALLET_OWNER_PASSWORD` / `WALLET_APP_PASSWORD` | `wallet_owner` / `wallet_app` | Senhas dos papéis `wallet_owner` (migrations) e `wallet_app` (aplicação) |
| `KEYCLOAK_ADMIN_USER` / `KEYCLOAK_ADMIN_PASSWORD` | `admin` | Administrador local do Keycloak |

Uma configuração inválida (por exemplo, `SHUTDOWN_TIMEOUT=abc`) encerra o processo na partida, com a lista de erros.

## Filas SQS

As filas são criadas automaticamente quando o LocalStack fica pronto, pelo script [deploy/localstack/init-queues.sh](deploy/localstack/init-queues.sh):

- `wager-transactions.fifo`: entrada de operações;
- `wager-transactions-dlq.fifo`: dead-letter queue.

*Pendente (Fase 7/8):* política de redrive, visibility timeout e o destino dos eventos de saída.

## Banco de dados e migrations

Na primeira inicialização do volume, o [deploy/postgres/init-roles.sh](deploy/postgres/init-roles.sh) cria o banco `wallet` e dois papéis:

| Papel | Uso | Privilégios |
|---|---|---|
| `wallet_owner` | migrations e testes de schema | dono do schema; `CREATEDB` para os bancos temporários dos testes |
| `wallet_app` | aplicação | `SELECT`, `INSERT` e `UPDATE` nas tabelas; **apenas `SELECT` e `INSERT` no ledger**; nenhum `DELETE` |

> O script só roda com o volume vazio. Se você já tinha o volume de uma versão anterior, recrie-o com `docker compose down -v`, que **apaga os dados locais**.

As migrations ficam em [migrations/](migrations/) e são aplicadas com `goose`. No `docker compose up`, o serviço `migrate` aplica as pendentes e termina; a aplicação só sobe depois que ele conclui com sucesso.

Comandos manuais pelo container `tools`, que já tem `GOOSE_DRIVER`, `GOOSE_DBSTRING` (papel `wallet_owner`) e `GOOSE_MIGRATION_DIR` configurados:

| Ação | Comando |
|---|---|
| Situação | `docker compose run --rm tools goose status` |
| Aplicar todas | `docker compose run --rm tools goose up` |
| Reverter a última | `docker compose run --rm tools goose down` |
| Reverter todas | `docker compose run --rm tools goose down-to 0` |

Forma nativa, com `goose` instalado:

```bash
goose -dir migrations postgres "postgres://wallet_owner:wallet_owner@localhost:5432/wallet?sslmode=disable" up
```

Essa forma exige publicar a porta 5432 do PostgreSQL, que não é publicada por padrão.

## Exemplos de chamadas

Health checks (públicos), a partir do container `tools`, sem nada instalado no host:

```bash
docker compose run --rm tools curl -s http://app:8080/health/live
```

```bash
docker compose run --rm tools curl -s http://app:8080/health/ready
```

- `/health/live` responde `200 {"status":"ok"}` enquanto o processo está de pé.
- `/health/ready` responde `200` com `{"status":"ok","checks":{"postgres":"ok","sqs":"ok"}}`, ou `503` quando alguma dependência está indisponível ou o processo está encerrando.

### Autenticação

As demais rotas exigem um access token `client_credentials` do Keycloak. O realm `wallet` é importado automaticamente na subida, com estes clients (segredos de uso local, no formato `<client>-secret`):

| Client | Provedor (`provider_id`) | Scopes | Pode |
|---|---|---|---|
| `wallet-backoffice` | — | `wallets:read`, `wallets:write`, `wallets:reconcile` | abrir e consultar carteiras, ledger e reconciliação |
| `provider-a` | `provider-a` | `wagering:write`, `wagering:read` | enviar e consultar as próprias operações |
| `provider-b` | `provider-b` | `wagering:write`, `wagering:read` | idem, para o provedor B |

Os clients `provider-short-lived` (token de 2 segundos) e `no-audience` (sem a audiência `wallet-api`) existem só para os testes de rejeição. O console administrativo fica em `http://localhost:8180` (`admin`/`admin`).

### Fluxo completo

Os comandos abaixo rodam dentro do container `tools`, sem nada instalado no host. Abra um shell nele:

```bash
docker compose run --rm tools bash
```

Dentro do shell, defina os endereços e uma função que obtém tokens:

```bash
KC=http://keycloak:8080/realms/wallet/protocol/openid-connect/token; API=http://app:8080
tok() { curl -s -u "$1:$1-secret" -d grant_type=client_credentials $KC | sed -E 's/.*"access_token":"([^"]+)".*/\1/'; }
BO=$(tok wallet-backoffice); PA=$(tok provider-a)
```

Abra uma carteira com 1000.00 (resposta `201`, com `id`, `playerId`, `balance` e `version: 1`):

```bash
curl -s -X POST $API/wallets -H "Authorization: Bearer $BO" -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

Guarde o `id` devolvido e envie a aposta de 25.00 (resposta `200`, `status: PROCESSED`, `balance: 975.00`). Repetir o mesmo comando devolve o mesmo resultado com `idempotentReplay: true`:

```bash
WALLET=<id devolvido>
curl -s -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"transaction-123\",\"playerId\":\"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
```

Consultas e reconciliação (a reconciliação devolve `consistent: true` e `checkedEntries: 2`):

```bash
curl -s $API/wallets/$WALLET -H "Authorization: Bearer $BO"
curl -s "$API/wallets/$WALLET/ledger?limit=50" -H "Authorization: Bearer $BO"
curl -s -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $BO"
curl -s $API/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PA"
```

**Pelo host:** os mesmos comandos funcionam com `KC=http://localhost:8180/realms/wallet/protocol/openid-connect/token` e `API=http://localhost:8080`. O emissor dos tokens é o mesmo nos dois casos.

### Respostas e erros

| Situação | Status |
|---|---|
| Operação processada | `200` |
| Operação rejeitada por regra de negócio (ex.: `INSUFFICIENT_FUNDS`), já persistida | `422`, com o corpo da transação e `failureCode` |
| Operação aguardando referência (`PENDING_REFERENCE`) | `202`, com `Location` para acompanhar |
| Replay | o mesmo status do original, com `idempotentReplay: true` |
| Entrada inválida (campo, valor, header `Idempotency-Key` ausente, campo desconhecido) | `400` |
| Token ausente, inválido ou expirado | `401` |
| Sem o scope, ou `providerId` diferente do token | `403` |
| Carteira ou transação inexistente (ou de outro provedor) | `404` |
| Mesma chave com outro conteúdo; mesma operação com outra chave; carteira duplicada | `409` |
| `Content-Type` diferente de `application/json` | `415` |
| Indisponibilidade temporária | `503`, com `Retry-After` |

Os erros seguem a RFC 9457 (`application/problem+json`), com `code` estável e `correlationId`. Envie `X-Correlation-Id` para correlacionar com os seus sistemas; se não enviar, o serviço gera um id e o devolve no mesmo header. O catálogo completo de códigos está em [ARCHITECTURE.md](ARCHITECTURE.md) (§4.9 e §4.13).

## Testes

Os testes unitários não precisam de nenhuma dependência no ar:

```bash
docker compose run --rm tools go test -race ./...
```

| Pacote | O que cobre |
|---|---|
| `internal/domain/money` | parsing estrito, escala, limites e overflow, entradas inválidas, incompatibilidade de moedas, JSON |
| `internal/domain/wallet` | invariantes da carteira, versão, abertura, reidratação, invariante do ledger |
| `internal/domain/wagering` | máquina de estados, regras dos cinco tipos externos, política de valor zero, reversões, referências pendentes, conflito de payload para a mesma chave, hash de idempotência, abertura interna |
| `internal/domain/event` | envelope, tipo e versão, formato de valores e datas |
| `internal/domain` | erros tipados, IDs, ausência de ponto flutuante, isolamento do domínio |
| `internal/app` | política de espera (backoff, teto, expiração), retry de falhas transitórias, cursor do ledger |
| `internal/adapters/httpapi` | header `Authorization`, JSON estrito, Money só como string, mapeamento de erros para HTTP, recuperação de panic |

### Testes de integração (persistência, casos de uso e API)

Os testes com a tag `integration` usam o PostgreSQL real do Compose. Cada pacote cria um banco temporário `wallet_test_<aleatório>` (helper em `internal/testutil/pgtest`), aplica as migrations como `wallet_owner`, executa os testes com o papel `wallet_app` e apaga o banco ao final. O banco da aplicação não é tocado.

```bash
docker compose up -d --wait postgres keycloak
```

```bash
docker compose run --rm tools go test -race -tags=integration ./internal/...
```

Forma nativa: exporte `DATABASE_URL` (papel `wallet_app`) e `DATABASE_OWNER_URL` (papel `wallet_owner`) apontando para o PostgreSQL e rode `go test -race -tags=integration ./internal/...`. Os testes da API também exigem `OIDC_ISSUER`, `OIDC_JWKS_URL` e `OIDC_TOKEN_URL` (já definidos no container `tools`).

**Persistência** (`internal/adapters/postgres`)

| Teste | O que comprova |
|---|---|
| `TestMigrationsApplyRevertAndReapply` | aplicação, reversão até a versão 0 e reaplicação |
| `TestLedgerIsAppendOnly` | `UPDATE`, `DELETE` e `TRUNCATE` no ledger barrados por privilégio (aplicação) e por trigger (dono do schema) |
| `TestBalanceChangeRequiresLedgerEntry` | saldo alterado sem lançamento não é confirmado |
| `TestLedgerChainIsEnforced` | cada lançamento continua do anterior (saldo e versão) |
| `TestWalletVersionRules`, `TestBalanceCannotBeNegative` | versão +1 só com mudança de saldo; saldo ≥ 0 |
| `TestDatabaseEnforcesDomainRules`, `TestTransactionStateMachineInDatabase` | regras dos tipos, códigos de falha e máquina de estados no banco |
| `TestIdempotencyConstraints`, `TestOneSuccessfulReversalPerReference` | unicidade por provedor, operação com outra chave, uma reversão por referência |
| `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100` | nenhuma atualização perdida sob concorrência (um processo; vários processos ficam para a Fase 10) |
| `TestIndependentWalletsDoNotBlock`, `TestLockTimeoutIsTransient` | lock só por carteira; espera excessiva vira erro transitório |
| `TestUnitOfWorkRollsBackEverything` | atomicidade: nada é gravado se a transação falha |
| `TestOutboxSnapshotIsImmutable`, `TestInboxDeduplicates` | snapshot imutável da outbox; deduplicação da inbox |

**Casos de uso** (`internal/app`, com relógio controlado)

| Teste | O que comprova |
|---|---|
| `TestOpenWallet` | abertura com 1000.00 (OPENING, lançamento e 2 eventos); saldo zero sem OPENING nem lançamento; segunda carteira do mesmo jogador e moeda em conflito |
| `TestReplayReturnsOriginalResult` | replay devolve o saldo observado no processamento original, mesmo depois de a carteira mudar |
| `TestIdempotencyConflicts` | mesma chave com outro conteúdo e mesma operação com outra chave resultam em conflito, sem efeito financeiro |
| `TestKeyNotConsumedByCorrectableErrors` | entrada inválida e carteira inexistente não persistem nada; a chave continua utilizável |
| `TestRejectionIsPersistedAndReplayed`, `TestLossHasNoMovement` | rejeição persistida e reproduzida; LOSS sem lançamento, sem nova versão e sem `WalletBalanceChanged` |
| `TestFiftyIdenticalBetsInParallel` | a mesma aposta 50 vezes em paralelo gera um único débito |
| `TestTwoBetsOf80OnWalletWith100` | uma processada, outra rejeitada por saldo, saldo final 20.00 e um único débito; reenvios não mudam o resultado |
| `TestIndependentWalletsInParallel` | carteiras distintas processadas simultaneamente |
| `TestReversalBeforeReferenceResolvedByWorker` | REFUND antes da BET fica pendente e é concluído pelo worker, com o correlationId original nos eventos |
| `TestPendingReferenceExpiresAfterMaxAttempts`, `TestPendingReferenceExpiresAfterTTL` | expiração com `REFERENCE_NOT_FOUND` e evento de rejeição |
| `TestConcurrentResolversProcessOnce` | várias tentativas simultâneas concluem a pendência uma única vez |
| `TestRefundAndRollbackOfTheSameBet`, `TestRollbackOfWinWithoutFunds` | devolução única por aposta; reversão sem saldo com código próprio |
| `TestReconciliation` | reconciliação consistente e detecção de divergência (log e resposta) sem alterar o saldo |
| `TestLedgerPagination`, `TestTransactionLookupsAreProviderScoped`, `TestSameKeyFromAnotherProviderIsIndependent` | cursor opaco e limites; consultas e replays isolados por provedor |

**API** (`internal/adapters/httpapi`, com o Keycloak real e tokens `client_credentials` de verdade)

| Teste | O que comprova |
|---|---|
| `TestSpecificationFlow` | os exemplos do enunciado: abertura de 1000.00, aposta de 25.00 (975.00), replay, reconciliação com `checkedEntries: 2`, consultas e paginação |
| `TestAuthenticationFailures` | token ausente, esquema não Bearer, malformado, assinatura adulterada, audiência errada e expirado resultam em `401`, sem efeito financeiro |
| `TestAuthorizationByScope` | cada client só acessa as rotas dos seus scopes (`403`), sem efeito financeiro |
| `TestProviderIsolation` | provedor não opera, não faz replay e não consulta em nome de outro; o `404` não vaza dados |
| `TestOperationResponses`, `TestInputAndConflictErrors` | `422` com o corpo da rejeição, `202` com `Location`, `400`, `404`, `409` e `415`; erros corrigíveis não consomem a chave |
| `TestWalletRules`, `TestCorrelationID` | abertura com zero, carteira duplicada, `X-Correlation-Id` propagado até a outbox |

### Testes de mutação

O [scripts/mutation.sh](scripts/mutation.sh) comprova que os testes verificam as regras de fato. Cada mutante injeta um defeito numa cópia descartável do código e algum teste precisa falhar. Exemplos de defeito: "débito deixa o saldo negativo", "segunda reversão aceita", "aplicação pode alterar e apagar o ledger", "lock da carteira sem FOR UPDATE", "worker ignora o horário agendado" e "audiência não conferida". Os mutantes de persistência, de casos de uso e de API exigem PostgreSQL e Keycloak no ar:

```bash
docker compose up -d --wait postgres keycloak
```

```bash
docker compose run --rm tools bash scripts/mutation.sh
```

Cada linha mostra o defeito e os testes que o detectaram (`KILLED`). `SURVIVED` indica regra sem teste, e `INVALID` indica mutante que não compila. Antes de cada grupo, os testes rodam sem mutação; se essa linha de base falhar (por exemplo, com o banco fora do ar), o script aborta em vez de contar falhas de ambiente como mutantes mortos. O script termina com erro se houver qualquer resultado diferente de `KILLED`.

## Testes de integração, múltiplas instâncias e falha

*Parcial.* Os testes de persistência estão descritos acima; os demais virão nas Fases 5 a 10. Os testes são separados por build tags:

| Tag | Conteúdo |
|---|---|
| *(nenhuma)* | unitários, sem infraestrutura |
| `integration` | PostgreSQL, Keycloak e LocalStack reais |
| `resilience` | ≥ 3 instâncias independentes, interrupções e indisponibilidade (via Toxiproxy) |

As dependências rodam no Compose, e as instâncias da aplicação são processos independentes dentro do container `tools`, sem montar o socket do Docker. A preparação e os comandos completos serão documentados aqui.

## Estrutura do repositório

```
cmd/wallet/            ponto de entrada (composição Fx)
internal/domain/       modelo de domínio puro
internal/app/          casos de uso, portas (UnitOfWork, repositórios) e erros
internal/adapters/     PostgreSQL, HTTP, SQS
internal/platform/     configuração, logs, health
deploy/                PostgreSQL (papéis), Keycloak, LocalStack, Toxiproxy
migrations/            schema, constraints e triggers (goose)
scripts/               testes de mutação
compose.yaml           stack local e container de toolchain
Dockerfile             imagens tools, build e runtime
```
