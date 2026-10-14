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
- [Observabilidade](#observabilidade)
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
| Métricas (Prometheus) | não publicado | `http://app:9090/metrics` |
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
| `SQS_WAGER_DLQ_NAME` | `wager-transactions-dlq.fifo` | Dead-letter queue |
| `SQS_CONSUMER_ENABLED` | `true` | Liga o consumidor (`false` para uma instância só HTTP) |
| `SQS_CONSUMER_POLLERS` | `2` | Loops de recebimento por instância |
| `SQS_WAIT_TIME` | `20s` | Long polling (máximo 20s) |
| `SQS_VISIBILITY_TIMEOUT` | `30s` | Visibilidade de cada recebimento, renovada durante o processamento |
| `SQS_RETRY_BACKOFF_BASE` / `SQS_RETRY_BACKOFF_MAX` | `2s` / `60s` | Backoff de falhas temporárias |
| `DB_MAX_CONNS` | `20` | Conexões do pool do PostgreSQL por instância |
| `SNS_EVENTS_TOPIC_ARN` | *(obrigatória com o publisher ligado)* | Tópico dos eventos de saída |
| `OUTBOX_PUBLISHER_ENABLED` | `true` | Liga o publisher da outbox |
| `OUTBOX_BATCH_SIZE` / `OUTBOX_LEASE` | `50` / `30s` | Eventos reservados por vez e duração da reserva (renovada a cada terço) |
| `OUTBOX_IDLE_INTERVAL` | `500ms` | Espera quando não há eventos pendentes |
| `OUTBOX_PUBLISH_CONCURRENCY` | `8` | Publicações simultâneas (agregados diferentes) |
| `OUTBOX_RETRY_BASE` / `OUTBOX_RETRY_MAX` | `1s` / `5m` | Backoff de falhas de publicação, sem limite de tentativas |
| `REFERENCE_WORKER_ENABLED` | `true` | Liga o worker de referências pendentes |
| `REFERENCE_WORKER_INTERVAL` / `REFERENCE_WORKER_BATCH` | `1s` / `50` | Ciclo e lote do worker |
| `REFERENCE_WORKER_CONCURRENCY` | `8` | Carteiras resolvidas em paralelo |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |
| `METRICS_ENABLED` | `true` | Liga as métricas Prometheus e o servidor de métricas |
| `METRICS_ADDR` | `:9090` | Endereço do servidor de métricas (`GET /metrics`), separado da API |
| `METRICS_REFRESH_INTERVAL` | `15s` | Ciclo de leitura dos gauges de backlog da outbox e de profundidade das DLQs |
| `SQS_EVENTS_DLQ_NAME` | `wallet-events-audit-dlq.fifo` | DLQ da assinatura dos eventos, monitorada junto com `SQS_WAGER_DLQ_NAME` |
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

| Fila | Configuração |
|---|---|
| `wager-transactions.fifo` | entrada de operações; FIFO, visibility timeout de 30s, redrive para a DLQ após 5 recebimentos |
| `wager-transactions-dlq.fifo` | dead-letter queue; retenção de 14 dias |

O script também registra as políticas de acesso: só o produtor (`wager-producer`) envia, e só o consumidor (`wallet-consumer`) recebe e remove. O LocalStack comunitário **não aplica** essas políticas; elas valem tal qual na AWS (detalhes em [ARCHITECTURE.md](ARCHITECTURE.md), §4.14).

### Contrato da mensagem

Envie com `MessageGroupId` igual ao `walletId` (ordem por carteira) e `MessageDeduplicationId` igual ao `messageId`:

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "<id da carteira>",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

Para enviar uma mensagem pelo container do LocalStack (sem nada instalado no host), troque `<id da carteira>` por uma carteira existente:

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "<id da carteira>" --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<id da carteira>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
```

### O que acontece com cada mensagem

| Resultado | Ação |
|---|---|
| Processada, rejeitada por regra de negócio ou aguardando referência | confirmada e removida da fila |
| Já recebida antes (mesmo `messageId` e mesmo conteúdo) | removida, sem reprocessar |
| Inválida, carteira inexistente, conflito ou mesmo `messageId` com outro conteúdo | enviada à DLQ com `ErrorCode` e `ErrorDetail`, e removida |
| Falha temporária (ex.: banco indisponível) | volta com backoff (2, 4, 8, 16s); na 5ª tentativa, vai para a DLQ pelo redrive |

Para inspecionar a DLQ:

```bash
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo
```

### Eventos de saída

Depois do commit de cada operação, o publisher da outbox envia os eventos ao tópico SNS FIFO `wallet-events.fifo`. A fila `wallet-events-audit.fifo` assina o tópico e recebe o envelope exatamente como gravado (entrega raw); entregas que falham vão para `wallet-events-audit-dlq.fifo`.

| Evento | Quando |
|---|---|
| `WagerTransactionProcessed` | operação concluída (inclusive `LOSS` e a abertura com saldo) |
| `WagerTransactionRejected` | rejeição definitiva por regra de negócio |
| `WalletBalanceChanged` | toda alteração efetiva de saldo |
| `WagerTransactionPendingReference` | operação passou a esperar a referência |

- **Roteamento:** `MessageGroupId` é o `aggregateId` (ordem por agregado), `MessageDeduplicationId` é o `eventId`, e os atributos `eventType` e `eventVersion` permitem filtrar por tipo.
- **Entrega:** at-least-once. Uma republicação usa sempre o mesmo `eventId`; depois da janela de 5 minutos de deduplicação do SNS, deduplique pelo `eventId`.

Para ver os eventos publicados:

```bash
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wallet-events-audit.fifo
```

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

## Observabilidade

### Logs

Uma linha JSON por evento, em `stdout`. Os ids da requisição ou da mensagem entram em todas as linhas do fluxo, sem precisar repassá-los a cada chamada:

| Atributo | Quando aparece |
|---|---|
| `correlationId` | requisições HTTP (do header `X-Correlation-Id` ou gerado) e eventos de pendências |
| `clientId`, `providerId` | depois da autenticação; `providerId` também nas operações recebidas por SQS |
| `walletId` | operações, pendências e reconciliação |
| `messageId` | mensagens do consumidor SQS |
| `transactionId` | tentativas do worker de referências |

Cada requisição HTTP gera uma linha `"http request"` com `method`, `route` (o padrão da rota, como `GET /wallets/{walletId}`, nunca a URL com ids), `status` e `durationMs`. Os health checks saem em `debug`, para não poluir o log; o resto, em `info`. Rotas inexistentes aparecem como `route: "unmatched"`.

Os logs **nunca** trazem tokens, o header `Authorization`, segredos ou valores monetários. A única exceção é a divergência de reconciliação, que registra os valores armazenado e calculado em `error`, porque são o próprio diagnóstico.

Exemplo:

```json
{"time":"2026-10-14T12:01:15.027Z","level":"INFO","msg":"http request","method":"POST","route":"POST /wallets","status":201,"durationMs":176,"clientId":"wallet-backoffice","correlationId":"01a13a49-c262-7e22-906b-b2cb94c2d488"}
```

### Métricas

As métricas ficam em `GET /metrics`, na porta `9090`, que **não é publicada no host**: só a rede interna do Compose as alcança. Para ler com a stack no ar:

```bash
docker compose run --rm tools curl -s http://app:9090/metrics
```

| Métrica | Tipo | Labels | O que mede |
|---|---|---|---|
| `wallet_operations_total` | counter | `channel`, `kind`, `status`, `failure_code` | operações concluídas de fato (replays não contam) |
| `wallet_operation_duration_seconds` | histograma | `channel`, `kind`, `outcome` | duração do caso de uso; `outcome` é o status ou o código do erro |
| `wallet_duplicates_total` | counter | `channel`, `reason` | `idempotent_replay` (mesma chave) e `inbox_redelivery` (mesma mensagem) |
| `wallet_retries_total` | counter | `component`, `reason` | novas tentativas do caso de uso, do consumidor e do publisher |
| `wallet_dead_letters_total` | counter | `queue`, `error_code` | mensagens enviadas à DLQ pelo consumidor |
| `wallet_dead_letter_queue_messages` | gauge | `queue` | mensagens nas DLQs de entrada e de eventos |
| `wallet_concurrency_conflicts_total` | counter | `kind` | `stale_version`, `lock_timeout`, `deadlock`, `serialization`, `unique` |
| `wallet_outbox_publish_delay_seconds` | histograma | — | tempo entre o evento e a sua publicação |
| `wallet_outbox_pending_events` | gauge | — | eventos ainda não publicados |
| `wallet_outbox_oldest_pending_age_seconds` | gauge | — | idade do evento pendente mais antigo |
| `wallet_reconciliations_total` | counter | `result` | `consistent` ou `divergent` |
| `wallet_pending_reference_attempts_total` | counter | `result` | tentativas do worker: `waiting`, `completed`, `expired` |

`channel` é `http`, `sqs` ou `worker`. Também são expostas as métricas padrão do runtime Go (`go_*`) e do processo (`process_*`). Nenhuma métrica carrega valor monetário (as métricas Prometheus são `float64`), nem ids de carteira ou de provedor, que gerariam cardinalidade sem limite.

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
| `internal/platform/logctx` | atributos do contexto em todas as linhas, substituição de chave repetida, contexto pai preservado |
| `internal/platform/metrics` | cada método da porta alimenta a métrica e os labels do catálogo |

### Testes de integração (persistência, casos de uso, API, SQS e workers)

Os testes com a tag `integration` usam o PostgreSQL real do Compose. Cada pacote cria um banco temporário `wallet_test_<aleatório>` (helper em `internal/testutil/pgtest`), aplica as migrations como `wallet_owner`, executa os testes com o papel `wallet_app` e apaga o banco ao final. O banco da aplicação não é tocado.

```bash
docker compose up -d --wait postgres keycloak localstack
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
| `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100` | nenhuma atualização perdida sob concorrência (com vários processos, veja a seção de resiliência) |
| `TestIndependentWalletsDoNotBlock`, `TestLockTimeoutIsTransient` | lock só por carteira; espera excessiva vira erro transitório |
| `TestUnitOfWorkRollsBackEverything` | atomicidade: nada é gravado se a transação falha |
| `TestOutboxSnapshotIsImmutable`, `TestInboxDeduplicates` | snapshot imutável da outbox; deduplicação da inbox |

Os testes `TestSaveDetectsStaleVersion`, `TestLockTimeoutIsTransient` e `TestIdempotencyConstraints` também conferem a métrica de conflito de cada caso (`stale_version`, `lock_timeout` e `unique`).

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
| `TestMetricsFiftyIdenticalBets` | 50 apostas iguais em paralelo: 1 operação, 49 duplicatas e 50 amostras de latência |
| `TestMetricsRejectionAndErrors`, `TestMetricsMessageChannel` | rejeição conta com o código; erro de validação só no histograma; reentrega da inbox e replay contados à parte |
| `TestMetricsPendingReferenceExpiration`, `TestMetricsReconciliation`, `TestMetricsUseCaseRetries` | tentativas e expiração do worker; resultado da reconciliação; cada retry pelo motivo da falha anterior |
| `TestLogsCarryIdsButNoMoney` | os fluxos principais registram ids pelo contexto e nenhum valor monetário |

**API** (`internal/adapters/httpapi`, com o Keycloak real e tokens `client_credentials` de verdade)

| Teste | O que comprova |
|---|---|
| `TestSpecificationFlow` | os exemplos do enunciado: abertura de 1000.00, aposta de 25.00 (975.00), replay, reconciliação com `checkedEntries: 2`, consultas e paginação |
| `TestAuthenticationFailures` | token ausente, esquema não Bearer, malformado, assinatura adulterada, audiência errada e expirado resultam em `401`, sem efeito financeiro |
| `TestAuthorizationByScope` | cada client só acessa as rotas dos seus scopes (`403`), sem efeito financeiro |
| `TestProviderIsolation` | provedor não opera, não faz replay e não consulta em nome de outro; o `404` não vaza dados |
| `TestOperationResponses`, `TestInputAndConflictErrors` | `422` com o corpo da rejeição, `202` com `Location`, `400`, `404`, `409` e `415`; erros corrigíveis não consomem a chave |
| `TestWalletRules`, `TestCorrelationID` | abertura com zero, carteira duplicada, `X-Correlation-Id` propagado até a outbox |
| `TestAccessLog` | uma linha por requisição com o padrão da rota, status, identidade e `correlationId`; health em `debug`; rota inexistente como `unmatched` |
| `TestLogsCarryNoCredentialsOrMoney` | nenhuma linha de log traz token, `Authorization`, segredo ou valor, inclusive em falhas |

**Consumidor SQS** (`internal/adapters/sqs`, com LocalStack real; cada teste cria o seu par de filas com redrive)

| Teste | O que comprova |
|---|---|
| `TestConsumesOperation` | mensagem do enunciado processada, inbox concluída, eventos com o `messageId` como correlationId |
| `TestRedeliveredMessageIsNotReprocessed`, `TestDifferentMessagesSameKey` | reentrega do mesmo envelope e mensagens diferentes com a mesma chave geram um único débito (deduplicação da aplicação, sem depender do SQS) |
| `TestHTTPAndSQSShareIdempotency` | a mesma operação por HTTP e por SQS, nas duas ordens, gera um único débito |
| `TestInvalidMessagesGoToDeadLetterQueue`, `TestMessageHashMismatchGoesToDeadLetterQueue` | 8 tipos de mensagem inválida e `messageId` reutilizado vão à DLQ com o `ErrorCode` certo (e a métrica por código), sem efeito financeiro |
| `TestBusinessOutcomesAreAcknowledged` | rejeição e espera por referência são confirmadas e removidas, sem DLQ |
| `TestTransientFailureIsRetried`, `TestExhaustedRetriesGoToDeadLetterQueue` | falha temporária volta com backoff (e conta como retry); após 5 recebimentos, o redrive move para a DLQ |
| `TestCrashAfterCommitBeforeDelete` | queda entre o commit e a remoção: a reentrega é reconhecida pela inbox e não reprocessa |
| `TestOrderWithinWallet`, `TestHeartbeatKeepsMessageInvisible` | ordem por carteira; processamento longo não deixa a mensagem reaparecer |
| `TestGracefulShutdown`, `TestStopDeadlineCancelsInFlight` | SIGTERM conclui a mensagem em andamento e devolve as não iniciadas; prazo estourado cancela sem gravar nada |

**Composição Fx** (`internal/bootstrap`)

| Teste | O que comprova |
|---|---|
| `TestCompositionLifecycle` | a composição real do `main` sobe, atende uma abertura e publica os eventos; ao parar, nenhum hook falha, a ordem é workers → servidor HTTP → consumidor → pool, o pool recusa conexões, a porta HTTP fica livre e o consumidor não recebe mais nada |

**Outbox e workers** (`internal/worker`, com SNS e SQS reais; cada teste cria o seu banco e o seu tópico)

| Teste | O que comprova |
|---|---|
| `TestEventsArePublishedAsCommitted`, `TestNothingIsPublishedBeforeCommit` | eventos chegam idênticos ao gravado, com grupo, tipo e versão; nada sai antes do commit |
| `TestOrderPerAggregate` | eventos de uma carteira saem na ordem de `walletVersion`, mesmo com falha e republicação no meio |
| `TestBrokerUnavailableIsRetriedWithBackoff` | falha no broker registra tentativa e erro, respeita o backoff e não perde evento; retries e atraso de publicação medidos |
| `TestGaugeLoopReportsBacklogAndDeadLetters` | gauges de backlog da outbox e de profundidade da DLQ; uma fila inexistente não interrompe as demais |
| `TestTwoPublishersCompete` | dois publishers na mesma outbox: cada evento publicado uma única vez |
| `TestAbandonedClaimIsRecovered`, `TestLeaseIsRenewedDuringSlowPublish` | reserva abandonada é assumida após o lease; reserva de um publisher vivo não é tomada |
| `TestRepublishAfterCrashKeepsEventID` | queda entre publicar e marcar: republicação com o mesmo `eventId` e os mesmos bytes, uma única cópia entregue |
| `TestStopReleasesUnstartedClaims`, `TestOutboxLoopLifecycle` | encerramento libera as reservas não iniciadas; o loop sobe, publica e para |
| `TestSubscriptionDeadLetters` | entrega do SNS que falha vai para a DLQ da assinatura |
| `TestReferenceWorkerResolvesPendingReversals`, `TestReferenceWorkerExpiresMissingReferences` | dois loops do worker concluem cada pendência uma única vez; sem referência, expira com `REFERENCE_NOT_FOUND` |

### Testes de mutação

O [scripts/mutation.sh](scripts/mutation.sh) comprova que os testes verificam as regras de fato. Cada mutante injeta um defeito numa cópia descartável do código e algum teste precisa falhar. Exemplos de defeito: "débito deixa o saldo negativo", "segunda reversão aceita", "aplicação pode alterar e apagar o ledger", "lock da carteira sem FOR UPDATE", "worker ignora o horário agendado", "audiência não conferida", "reserva ignora a ordem por agregado" e "access log registra o header Authorization". Os mutantes de persistência, de casos de uso, de API, do consumidor e dos workers exigem PostgreSQL, Keycloak e LocalStack no ar:

```bash
docker compose up -d --wait postgres keycloak localstack
```

```bash
docker compose run --rm tools bash scripts/mutation.sh
```

Cada linha mostra o defeito e os testes que o detectaram (`KILLED`). `SURVIVED` indica regra sem teste, e `INVALID` indica mutante que não compila. Antes de cada grupo, os testes rodam sem mutação; se essa linha de base falhar (por exemplo, com o banco fora do ar), o script aborta em vez de contar falhas de ambiente como mutantes mortos. O script termina com erro se houver qualquer resultado diferente de `KILLED`. Para rodar só um grupo, filtre pelo nome do mutante, por exemplo: `docker compose run --rm -e MUTATION_FILTER="inbox|DLQ" tools bash scripts/mutation.sh`.

O grupo de resiliência roda 4 mutantes contra os 3 processos reais, cada um só com o cenário que o detecta. Ele é opcional porque leva alguns minutos e exige a stack inteira no ar:

```bash
docker compose run --rm -e MUTATION_RESILIENCE=1 tools bash scripts/mutation.sh
```

## Testes de integração, múltiplas instâncias e falha

Os testes são separados por build tags:

| Tag | Conteúdo | Pré-requisito |
|---|---|---|
| *(nenhuma)* | unitários | nada |
| `integration` | PostgreSQL, Keycloak e LocalStack reais (descritos acima) | `docker compose up -d --wait postgres keycloak localstack` |
| `resilience` | 3 processos reais da aplicação, quedas com SIGKILL e SIGTERM, indisponibilidade com o Toxiproxy | a stack inteira: `docker compose up -d --wait` |

As dependências rodam no Compose. As instâncias da aplicação são **processos independentes dentro do container `tools`**, cada um com suas conexões e memória, sem montar o socket do Docker. O teste compila o binário (com `-race`) e sobe 3 cópias, cada uma com as suas portas. Cada teste usa um banco temporário e filas e tópico próprios, então não interfere no app do Compose nem nos outros testes. As instâncias falam com o PostgreSQL e o LocalStack pelo Toxiproxy, que o teste liga e desliga pela API HTTP (`TOXIPROXY_URL`, já definida no container `tools`).

Preparação:

```bash
docker compose up --build -d --wait
```

Execução (2 a 3 minutos, incluindo a compilação com `-race`):

```bash
docker compose run --rm tools go test -tags=resilience -v ./test/resilience/
```

Para rodar um cenário só, use `-run`, por exemplo `-run TestKillDuringLoad`. Se um teste falhar, as últimas linhas do log de cada instância são impressas. Os logs também são varridos em busca de `panic` e `DATA RACE`.

| Teste | Cenário com 3 processos |
|---|---|
| `TestFiftyIdenticalBetsAcrossInstances` | a mesma aposta 50 vezes, espalhada pelas instâncias: um único débito |
| `TestTwoBetsOf80AcrossInstances` | duas apostas de 80.00 numa carteira de 100.00 em processos diferentes: uma processada, uma rejeitada, saldo 20.00; reenvios não mudam nada |
| `TestBurstOnOneWalletAcrossInstances` | 60 apostas distintas na mesma carteira ao mesmo tempo: todas aplicadas, sem nenhum conflito (o lock serializa os processos) |
| `TestIndependentWalletsAcrossInstances` | muitas carteiras em paralelo pelos 3 processos |
| `TestHTTPAndSQSAcrossInstances` | a mesma operação por HTTP e por SQS, com mensagens repetidas: um único débito |
| `TestWorkersAcrossInstances` | 3 publishers na mesma outbox e 3 workers nas mesmas pendências: cada evento e cada reversão uma única vez |
| `TestKillDuringLoad` | SIGKILL numa instância no meio da carga; ela volta depois |
| `TestRestartAllInstances` | as 3 caem juntas; 3 novas devolvem os mesmos replays, retomam pendências e publicam os eventos pendentes |
| `TestKillConsumerAfterCommit` | SIGKILL entre o commit e a remoção da mensagem: a reentrega é barrada pela inbox |
| `TestKillPublisherHoldingClaims` | SIGKILL no publisher com eventos reservados: outro assume após o lease, sem duplicar |
| `TestGracefulTermination` | SIGTERM sob carga: saída com código 0, nada se perde |
| `TestDatabaseOutage` | PostgreSQL fora por 5 s: `503`, readiness falhando e recuperação sem duplicidade nem DLQ |
| `TestBrokerOutage` | SQS e SNS fora por 6 s: a API continua, os eventos esperam na outbox e são publicados na volta |

Todo cenário termina conferindo que o saldo de cada carteira bate com o ledger, que nenhuma transação tem dois lançamentos e que cada evento da outbox chegou exatamente uma vez à fila assinante. Nas instâncias de teste, os tempos (visibilidade, lease, backoff) são encurtados por variáveis de ambiente, para os cenários rodarem em segundos; o código é o mesmo da produção. Detalhes em [ARCHITECTURE.md](ARCHITECTURE.md) (§4.19).

### Limitações conhecidas dos testes de resiliência

- **Duas reversões simultâneas da mesma aposta em processos diferentes** não têm um teste multi-instância dedicado. A regra (no máximo uma reversão bem-sucedida por aposta) é garantida pelo índice único `wager_transactions_one_reversal_uq` no banco e testada com um processo (`TestOneSuccessfulReversalPerReference`, `TestRefundAndRollbackOfTheSameBet`).
- **A expiração de referência pendente** (`REFERENCE_NOT_FOUND`) é testada com um processo (`TestPendingReferenceExpiresAfterMaxAttempts`, `TestPendingReferenceExpiresAfterTTL`, `TestReferenceWorkerExpiresMissingReferences`). Com 3 processos, só a resolução das pendências é testada. A conclusão única entre várias instâncias usa o mesmo lock e a mesma checagem de agendamento nos dois casos.
- As 3 instâncias rodam no mesmo container: são processos independentes, com memória e conexões próprias, mas compartilham CPU e rede.
- A queda entre o commit e a remoção da mensagem é provocada sob carga, e o teste exige evidência de que a janela foi atingida. O ponto exato é coberto de forma determinística pelos testes de integração do consumidor.

As demais limitações do projeto estão em [ARCHITECTURE.md](ARCHITECTURE.md) (§7).

## Estrutura do repositório

```
cmd/wallet/            ponto de entrada: carrega a configuração e sobe a composição
internal/bootstrap/    raiz de composição Fx (usada pelo main e pelos testes)
internal/domain/       modelo de domínio puro
internal/app/          casos de uso, portas (UnitOfWork, repositórios) e erros
internal/adapters/     PostgreSQL, HTTP, SQS
internal/platform/     configuração, logs (com atributos de contexto), health, métricas
deploy/                PostgreSQL (papéis), Keycloak, LocalStack, Toxiproxy
migrations/            schema, constraints e triggers (goose)
scripts/               testes de mutação
test/resilience/       testes com 3 processos reais, SIGKILL e Toxiproxy
compose.yaml           stack local e container de toolchain
Dockerfile             imagens tools, build e runtime
```
