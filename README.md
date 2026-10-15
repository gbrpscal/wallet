# Wallet

Serviço em Go para carteiras de jogadores e operações de apostas (wagering). Recebe operações por uma API HTTP e por uma fila SQS FIFO, e as duas entradas passam pelo mesmo caso de uso, com as mesmas regras de saldo, concorrência e idempotência.

As decisões de arquitetura, as limitações e a matriz que liga cada exigência do enunciado ao código e aos testes estão em [ARCHITECTURE.md](ARCHITECTURE.md).

## Sumário

- [Guia rápido](#guia-rápido)
- [Pré-requisitos](#pré-requisitos)
- [Container de toolchain](#container-de-toolchain)
- [Execução da aplicação](#execução-da-aplicação)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Banco de dados e migrations](#banco-de-dados-e-migrations)
- [Exemplos de chamadas](#exemplos-de-chamadas)
- [Filas SQS](#filas-sqs)
- [Observabilidade](#observabilidade)
- [Testes](#testes)
- [Testes de integração, múltiplas instâncias e falha](#testes-de-integração-múltiplas-instâncias-e-falha)
- [Solução de problemas](#solução-de-problemas)
- [Estrutura do repositório](#estrutura-do-repositório)

## Guia rápido

Só é preciso ter Docker. Na raiz do projeto:

1. Suba o ambiente. Na primeira execução as imagens são baixadas e compiladas, o que leva alguns minutos.

   ```bash
   docker compose up --build -d --wait
   ```

   O comando volta quando `postgres`, `keycloak`, `localstack` e `app` estão saudáveis. As migrations, as filas, o tópico de eventos e o realm do Keycloak são criados automaticamente.

2. Confira se a aplicação está pronta. A resposta deve trazer `"postgres":"ok"` e `"sqs":"ok"`.

   ```bash
   docker compose run --rm tools curl -s http://app:8080/health/ready
   ```

3. Rode o fluxo do enunciado (abertura, aposta, reenvio e reconciliação) com o bloco da seção [Fluxo completo](#fluxo-completo).
4. Envie uma operação pela fila, como em [Filas SQS](#filas-sqs).
5. Veja as métricas, conforme [Observabilidade](#observabilidade).
6. Rode os testes, conforme [Testes](#testes).
7. Para encerrar, use `docker compose down`. Com `docker compose down -v`, o banco e os caches também são apagados.

## Pré-requisitos

Docker com Compose. O projeto foi testado com Docker 29 e Compose v5. Go, `goose` e os demais utilitários rodam em containers, então não é preciso instalar nada no host. Quem preferir pode usar Go 1.27.2 localmente.

## Container de toolchain

O serviço `tools` do Compose tem Go 1.27.2 (com CGO, necessário para `-race`) e `goose`. O código é montado como volume, e os caches de módulos e de build ficam em volumes nomeados. Ele só sobe quando é chamado com `docker compose run`.

| Ação | No container | Nativo |
|---|---|---|
| Compilar | `docker compose run --rm tools go build ./...` | `go build ./...` |
| Testes unitários | `docker compose run --rm tools go test ./...` | `go test ./...` |
| Testes com race detector | `docker compose run --rm tools go test -race ./...` | `go test -race ./...` |
| Análise estática | `docker compose run --rm tools go vet ./...` | `go vet ./...` |
| Formatação | `docker compose run --rm tools gofmt -l .` | `gofmt -l .` |

O container roda como UID/GID `1000:1000`, para que arquivos gerados pertençam ao usuário do host. Se o seu UID for outro, copie `.env.example` para `.env` e ajuste `HOST_UID` e `HOST_GID` com a saída de `id -u` e `id -g`.

## Execução da aplicação

```bash
docker compose up --build
```

Sobe PostgreSQL, Keycloak, LocalStack, Toxiproxy e a aplicação. Para rodar em segundo plano e esperar os health checks, acrescente `-d --wait`. O serviço `migrate` aplica as migrations e termina antes de a aplicação subir.

| Serviço | No host | Na rede do Compose |
|---|---|---|
| API | `http://localhost:8080` | `http://app:8080` |
| Métricas | não publicado | `http://app:9090/metrics` |
| Keycloak | `http://localhost:8180` | `http://keycloak:8080` |
| PostgreSQL | não publicado | `postgres:5432` |
| LocalStack | não publicado | `http://localstack:4566` |
| Toxiproxy | não publicado | `toxiproxy:8474` |

A imagem do LocalStack está fixada em `4.14.0`, a última versão comunitária que funciona sem conta; as imagens mais novas exigem `LOCALSTACK_AUTH_TOKEN`. Para trocar de versão, altere a tag no `compose.yaml` e defina o token no serviço.

## Variáveis de ambiente

O projeto funciona sem `.env`: os valores padrão estão no `compose.yaml`. O `.env.example` traz os valores que podem ser sobrescritos, todos de uso local. Uma configuração inválida (por exemplo, `SHUTDOWN_TIMEOUT=abc`) faz o processo encerrar na partida com a lista de erros.

Aplicação:

| Variável | Padrão | Descrição |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Endereço do servidor HTTP |
| `DATABASE_URL` | obrigatória | Conexão com o PostgreSQL, com o papel `wallet_app` |
| `DB_MAX_CONNS` | `20` | Conexões do pool por instância |
| `DB_LOCK_TIMEOUT` | `5s` | Espera máxima por um lock de linha; ao estourar, a operação falha como transitória |
| `OIDC_ISSUER` | obrigatória | Emissor esperado nos tokens (`http://localhost:8180/realms/wallet` no Compose) |
| `OIDC_JWKS_URL` | obrigatória | Chaves públicas do Keycloak |
| `OIDC_AUDIENCE` | `wallet-api` | Audiência exigida nos tokens |
| `AWS_REGION` | `us-east-1` | Região |
| `AWS_ENDPOINT_URL` | — | Endpoint do LocalStack |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `test` / `test` | Credenciais, lidas pelo SDK da AWS |
| `SQS_WAGER_QUEUE_NAME` / `SQS_WAGER_DLQ_NAME` | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` | Fila de entrada e sua DLQ |
| `SQS_CONSUMER_ENABLED` | `true` | Liga o consumidor |
| `SQS_CONSUMER_POLLERS` | `2` | Loops de recebimento por instância |
| `SQS_WAIT_TIME` | `20s` | Long polling (máximo 20s) |
| `SQS_VISIBILITY_TIMEOUT` | `30s` | Visibilidade de cada mensagem, renovada durante o processamento |
| `SQS_RETRY_BACKOFF_BASE` / `SQS_RETRY_BACKOFF_MAX` | `2s` / `60s` | Backoff de falhas temporárias |
| `SNS_EVENTS_TOPIC_ARN` | obrigatória com o publisher ligado | Tópico dos eventos de saída |
| `SQS_EVENTS_DLQ_NAME` | `wallet-events-audit-dlq.fifo` | DLQ da assinatura de eventos |
| `OUTBOX_PUBLISHER_ENABLED` | `true` | Liga o publisher da outbox |
| `OUTBOX_BATCH_SIZE` / `OUTBOX_LEASE` | `50` / `30s` | Eventos reservados por vez e duração da reserva |
| `OUTBOX_IDLE_INTERVAL` | `500ms` | Espera quando não há eventos pendentes |
| `OUTBOX_PUBLISH_CONCURRENCY` | `8` | Publicações simultâneas |
| `OUTBOX_RETRY_BASE` / `OUTBOX_RETRY_MAX` | `1s` / `5m` | Backoff de falhas de publicação, sem limite de tentativas |
| `REFERENCE_WORKER_ENABLED` | `true` | Liga o worker de referências pendentes |
| `REFERENCE_WORKER_INTERVAL` / `REFERENCE_WORKER_BATCH` | `1s` / `50` | Ciclo e lote do worker |
| `REFERENCE_WORKER_CONCURRENCY` | `8` | Carteiras resolvidas em paralelo |
| `REFERENCE_MAX_ATTEMPTS` / `REFERENCE_TTL` | `10` / `10m` | Limite de espera por uma referência; o que vier primeiro encerra com `REFERENCE_NOT_FOUND` |
| `REFERENCE_BACKOFF_BASE` / `REFERENCE_BACKOFF_MAX` | `1s` / `60s` | Backoff entre tentativas, com ±20% de jitter |
| `METRICS_ENABLED` / `METRICS_ADDR` | `true` / `:9090` | Servidor de métricas, separado da API |
| `METRICS_REFRESH_INTERVAL` | `15s` | Atualização dos gauges de outbox e DLQ |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |
| `SHUTDOWN_TIMEOUT` | `30s` | Prazo para terminar o trabalho em andamento no encerramento |
| `READINESS_TIMEOUT` | `2s` | Timeout de cada checagem de readiness |
| `HTTP_READ_TIMEOUT` / `HTTP_WRITE_TIMEOUT` / `HTTP_READ_HEADER_TIMEOUT` | `10s` / `15s` / `5s` | Timeouts do servidor HTTP |

Compose (`.env`):

| Variável | Padrão | Descrição |
|---|---|---|
| `HOST_UID` / `HOST_GID` | `1000` | Usuário do container `tools` |
| `APP_HOST_PORT` / `KEYCLOAK_HOST_PORT` | `8080` / `8180` | Portas publicadas no host |
| `POSTGRES_PASSWORD` | `postgres` | Superusuário, usado só para criar os papéis |
| `WALLET_DB` | `wallet` | Nome do banco |
| `WALLET_OWNER_PASSWORD` / `WALLET_APP_PASSWORD` | `wallet_owner` / `wallet_app` | Senhas dos papéis de migrations e da aplicação |
| `KEYCLOAK_ADMIN_USER` / `KEYCLOAK_ADMIN_PASSWORD` | `admin` / `admin` | Administrador do Keycloak |

## Banco de dados e migrations

Na primeira subida do volume, o script [deploy/postgres/init-roles.sh](deploy/postgres/init-roles.sh) cria o banco `wallet` e dois papéis. O `wallet_owner` é dono do schema e aplica as migrations. O `wallet_app` é o papel da aplicação: tem `SELECT`, `INSERT` e `UPDATE` nas tabelas, mas no ledger só `SELECT` e `INSERT`, e não tem `DELETE` em nada. O script só roda com o volume vazio; para recriá-lo, use `docker compose down -v`, que apaga os dados locais.

As migrations ficam em [migrations/](migrations/) e são aplicadas com `goose`. O container `tools` já vem com a conexão configurada:

```bash
docker compose run --rm tools goose status
docker compose run --rm tools goose up
docker compose run --rm tools goose down
docker compose run --rm tools goose down-to 0
```

`goose down` reverte a última migration e `down-to 0` reverte todas. Para rodar o `goose` fora do container, é preciso publicar a porta 5432 do PostgreSQL, que fica fechada por padrão.

## Exemplos de chamadas

### Autenticação

Todas as rotas, exceto `/health/live` e `/health/ready`, exigem um token `client_credentials` do Keycloak. O realm `wallet` é importado na subida com estes clients, cujos segredos seguem o formato `<client>-secret`:

| Client | Provedor | Scopes | Uso |
|---|---|---|---|
| `wallet-backoffice` | — | `wallets:read`, `wallets:write`, `wallets:reconcile` | abrir e consultar carteiras, ledger e reconciliação |
| `provider-a` | `provider-a` | `wagering:read`, `wagering:write` | enviar e consultar as próprias operações |
| `provider-b` | `provider-b` | `wagering:read`, `wagering:write` | o mesmo, para o provedor B |

Os clients `provider-short-lived` (token de 2 segundos) e `no-audience` (token sem a audiência `wallet-api`) existem para os testes de rejeição. O console do Keycloak fica em `http://localhost:8180`, com `admin`/`admin`.

### Fluxo completo

Os comandos abaixo rodam dentro do container `tools`. Abra um shell nele:

```bash
docker compose run --rm tools bash
```

e cole o bloco a seguir. Ele obtém os tokens, abre uma carteira com 1000.00, envia a mesma aposta de 25.00 duas vezes e reconcilia a carteira. Como cada execução cria um jogador novo, o bloco pode ser repetido.

```bash
KC=http://keycloak:8080/realms/wallet/protocol/openid-connect/token; API=http://app:8080
tok() { curl -s -u "$1:$1-secret" -d grant_type=client_credentials $KC | sed -E 's/.*"access_token":"([^"]+)".*/\1/'; }
BO=$(tok wallet-backoffice); PA=$(tok provider-a); PLAYER=$(cat /proc/sys/kernel/random/uuid)
WALLET=$(curl -s -X POST $API/wallets -H "Authorization: Bearer $BO" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
BET="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"bet-$PLAYER\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
for i in 1 2; do curl -s -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:bet-$PLAYER" -d "$BET"; echo; done
curl -s -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $BO"; echo
```

A primeira aposta volta com `"status":"PROCESSED"`, saldo `975.00` e `"idempotentReplay":false`. A segunda devolve o mesmo `transactionId`, agora com `"idempotentReplay":true`, sem novo débito. A reconciliação mostra `"consistent":true` e `"checkedEntries":2`.

Na mesma sessão, as outras consultas:

```bash
curl -s $API/wallets/$WALLET -H "Authorization: Bearer $BO"
curl -s "$API/wallets/$WALLET/ledger?limit=50" -H "Authorization: Bearer $BO"
curl -s $API/providers/provider-a/wagering/transactions/bet-$PLAYER -H "Authorization: Bearer $PA"
```

Para reversões (`REFUND` e `ROLLBACK`), acrescente `referenceExternalTransactionId` ao corpo. Os mesmos comandos funcionam a partir do host trocando os endereços por `KC=http://localhost:8180/realms/wallet/protocol/openid-connect/token` e `API=http://localhost:8080`.

### Respostas e erros

| Situação | Status |
|---|---|
| Operação processada | `200` |
| Rejeição de negócio, como `INSUFFICIENT_FUNDS` (já persistida) | `422`, com a transação e o `failureCode` |
| Aguardando a transação referenciada (`PENDING_REFERENCE`) | `202`, com `Location` |
| Reenvio com a mesma chave e o mesmo conteúdo | o status original, com `idempotentReplay: true` |
| Entrada inválida ou `Idempotency-Key` ausente | `400` |
| Token ausente, inválido ou expirado | `401` |
| Sem o scope, ou `providerId` diferente do token | `403` |
| Carteira ou transação inexistente, ou de outro provedor | `404` |
| Mesma chave com outro conteúdo, mesma operação com outra chave, carteira duplicada | `409` |
| `Content-Type` diferente de `application/json` | `415` |
| Indisponibilidade temporária | `503`, com `Retry-After` |

Os erros seguem a RFC 9457 (`application/problem+json`) e trazem um `code` estável e o `correlationId`. O header `X-Correlation-Id` enviado pelo cliente é aproveitado; sem ele, o serviço gera um id e o devolve no mesmo header. O catálogo de códigos está no [ARCHITECTURE.md](ARCHITECTURE.md), §4.9 e §4.13.

## Filas SQS

O script [deploy/localstack/init-queues.sh](deploy/localstack/init-queues.sh) cria as filas quando o LocalStack fica pronto:

- `wager-transactions.fifo`: entrada de operações, com visibility timeout de 30s e redrive para a DLQ após 5 recebimentos;
- `wager-transactions-dlq.fifo`: dead-letter queue, com retenção de 14 dias.

O mesmo script registra as políticas de acesso das filas: só o produtor envia e só o consumidor recebe e remove. O LocalStack comunitário não aplica essas políticas; elas valem como estão na AWS.

As mensagens usam `MessageGroupId` igual ao `walletId`, o que mantém a ordem por carteira, e `MessageDeduplicationId` igual ao `messageId`. O corpo segue o formato do enunciado:

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

Para enviar uma mensagem pelo próprio container do LocalStack, troque `<id da carteira>` pelo id de uma carteira existente:

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "<id da carteira>" --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<id da carteira>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
```

O que o consumidor faz com cada mensagem:

| Mensagem | Destino |
|---|---|
| Processada, rejeitada por regra de negócio ou aguardando referência | confirmada e removida da fila |
| Já recebida, com o mesmo `messageId` e o mesmo conteúdo | removida sem reprocessar |
| Inválida, carteira inexistente, conflito ou `messageId` reutilizado com outro conteúdo | enviada à DLQ com `ErrorCode` e `ErrorDetail` |
| Falha temporária, como banco indisponível | volta com backoff de 2, 4, 8 e 16s; no 5º recebimento, o redrive a leva para a DLQ |

Para ler a DLQ:

```bash
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo
```

### Eventos de saída

Depois do commit de cada operação, o publisher da outbox envia os eventos ao tópico SNS FIFO `wallet-events.fifo`. A fila `wallet-events-audit.fifo` assina o tópico e recebe o envelope exatamente como foi gravado; as entregas que falham vão para `wallet-events-audit-dlq.fifo`.

São quatro eventos: `WagerTransactionProcessed` (operação concluída, inclusive `LOSS` e a abertura com saldo), `WagerTransactionRejected` (rejeição de negócio), `WalletBalanceChanged` (toda mudança de saldo) e `WagerTransactionPendingReference` (operação esperando a referência). O `MessageGroupId` é o `aggregateId` e o `MessageDeduplicationId` é o `eventId`. A entrega é at-least-once e uma republicação mantém o mesmo `eventId`, então, passada a janela de 5 minutos de deduplicação do SNS, o consumidor deve deduplicar por ele.

```bash
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wallet-events-audit.fifo
```

## Observabilidade

Os logs são JSON, uma linha por evento, em `stdout`. Os identificadores do fluxo (`correlationId`, `messageId`, `transactionId`, `walletId`, `providerId` e `clientId`) entram em todas as linhas por meio do contexto. Cada requisição HTTP gera uma linha `"http request"` com o padrão da rota (por exemplo, `GET /wallets/{walletId}`), o status e a duração; os health checks são registrados em `debug`. Tokens, o header `Authorization` e valores monetários não aparecem nos logs. A única exceção é o log de divergência de reconciliação, que registra os saldos armazenado e calculado.

```json
{"time":"2026-10-14T12:01:15.027Z","level":"INFO","msg":"http request","method":"POST","route":"POST /wallets","status":201,"durationMs":176,"clientId":"wallet-backoffice","correlationId":"01a13a49-c262-7e22-906b-b2cb94c2d488"}
```

As métricas, no formato Prometheus, ficam em `GET /metrics` na porta 9090, acessível apenas pela rede interna do Compose:

```bash
docker compose run --rm tools sh -c "curl -s http://app:9090/metrics | grep '^wallet_' | grep -v _bucket"
```

| Métrica | O que mede |
|---|---|
| `wallet_operations_total` | operações concluídas, por canal, tipo, status e código de falha |
| `wallet_operation_duration_seconds` | latência do caso de uso |
| `wallet_duplicates_total` | reenvios barrados pela chave de idempotência ou pela inbox |
| `wallet_retries_total` | novas tentativas do caso de uso, do consumidor e do publisher |
| `wallet_dead_letters_total`, `wallet_dead_letter_queue_messages` | mensagens enviadas às DLQs e quantas estão nelas |
| `wallet_concurrency_conflicts_total` | conflitos de versão, lock timeout, deadlock e unicidade |
| `wallet_outbox_publish_delay_seconds`, `wallet_outbox_pending_events`, `wallet_outbox_oldest_pending_age_seconds` | atraso e acúmulo da outbox |
| `wallet_reconciliations_total` | reconciliações consistentes e divergentes |
| `wallet_pending_reference_attempts_total` | tentativas do worker de referências |

Nenhuma métrica carrega valor monetário ou ids de carteira e provedor. Os health checks são `/health/live` (o processo está de pé) e `/health/ready`, que verifica PostgreSQL e SQS e responde `503` quando algum deles está indisponível ou durante o encerramento.

## Testes

Os testes são separados por build tags:

| Tag | O que roda | Precisa de |
|---|---|---|
| nenhuma | testes unitários | nada |
| `integration` | testes com PostgreSQL, Keycloak e LocalStack reais | `docker compose up -d --wait postgres keycloak localstack` |
| `resilience` | testes com 3 processos da aplicação, quedas e indisponibilidade | a stack inteira: `docker compose up --build -d --wait` |

```bash
docker compose run --rm tools go test -race ./...
docker compose run --rm tools go test -race -tags=integration ./internal/...
```

Os testes unitários cobrem o domínio: `Money` (parsing, escala, overflow, moedas incompatíveis), as invariantes da carteira, a máquina de estados, as regras dos cinco tipos de operação, a política de valores zero, o hash de idempotência e a abertura interna. Também verificam que nenhum tipo de ponto flutuante aparece no código de produção e que o domínio não importa bibliotecas de infraestrutura.

Os testes de integração criam um banco temporário por pacote (`wallet_test_<aleatório>`), aplicam as migrations e rodam com o papel da aplicação; o banco `wallet` não é tocado. Eles cobrem:

- persistência: migrations com aplicação e reversão, constraints, ledger imutável, atomicidade e concorrência (`internal/adapters/postgres`);
- casos de uso: idempotência, replay com o saldo original, rejeições, referências pendentes, reconciliação e métricas (`internal/app`);
- API com tokens reais do Keycloak: autenticação, scopes, isolamento entre provedores e códigos de resposta (`internal/adapters/httpapi`);
- consumidor SQS: inbox, reentrega, DLQ, backoff, heartbeat e encerramento (`internal/adapters/sqs`);
- outbox e workers: publicação só após o commit, disputa entre publishers, recuperação e ordem por carteira (`internal/worker`);
- composição Fx: a aplicação real sobe, processa uma abertura e, ao parar, encerra os componentes em ordem e libera as conexões (`internal/bootstrap`).

A lista de testes por exigência está na matriz do [ARCHITECTURE.md](ARCHITECTURE.md), §8. Fora do container, os testes de integração precisam de `DATABASE_URL`, `DATABASE_OWNER_URL`, `OIDC_ISSUER`, `OIDC_JWKS_URL` e `OIDC_TOKEN_URL`, que o container `tools` já define.

### Testes de mutação

O script [scripts/mutation.sh](scripts/mutation.sh) injeta defeitos conhecidos, um por vez, numa cópia do código, como remover o `FOR UPDATE` da carteira ou aceitar uma segunda reversão, e confere se algum teste falha. Cada linha da saída mostra o defeito e o teste que o detectou. Com a stack no ar, a execução leva de 20 a 30 minutos:

```bash
docker compose run --rm tools bash scripts/mutation.sh
```

O script para com erro se algum defeito passar despercebido ou se os testes falharem antes da mutação, por exemplo com o banco fora do ar. `MUTATION_FILTER` restringe a execução por nome (`-e MUTATION_FILTER="inbox|DLQ"`), e `MUTATION_RESILIENCE=1` inclui os mutantes que rodam contra os 3 processos.

## Testes de integração, múltiplas instâncias e falha

Os testes com a tag `resilience` (em `test/resilience`) compilam a aplicação com `-race` e sobem três processos independentes dentro do container `tools`, cada um com suas portas, conexões e memória. Cada teste usa um banco, filas e um tópico próprios, sem interferir no app do Compose. As instâncias acessam o PostgreSQL e o LocalStack pelo Toxiproxy, que o teste desliga para simular indisponibilidade. O socket do Docker não é montado.

Com a stack inteira no ar, a suíte leva de 2 a 3 minutos:

```bash
docker compose up --build -d --wait
docker compose run --rm tools go test -tags=resilience -v ./test/resilience/
```

Um cenário isolado roda com `-run`, por exemplo `-run TestKillDuringLoad`. Quando um teste falha, as últimas linhas do log de cada instância são impressas.

| Teste | Cenário |
|---|---|
| `TestFiftyIdenticalBetsAcrossInstances` | a mesma aposta 50 vezes, espalhada entre as instâncias: um único débito |
| `TestTwoBetsOf80AcrossInstances` | duas apostas de 80.00 numa carteira de 100.00, em processos diferentes: uma processada, outra rejeitada, saldo 20.00 |
| `TestBurstOnOneWalletAcrossInstances` | 60 apostas na mesma carteira ao mesmo tempo, sem nenhum conflito de concorrência |
| `TestIndependentWalletsAcrossInstances` | várias carteiras em paralelo |
| `TestHTTPAndSQSAcrossInstances` | a mesma operação por HTTP e por SQS, com mensagens repetidas: um único débito |
| `TestWorkersAcrossInstances` | três publishers e três workers de referências ao mesmo tempo |
| `TestKillDuringLoad` | uma instância derrubada com SIGKILL no meio da carga |
| `TestRestartAllInstances` | as três instâncias derrubadas juntas e substituídas por novas |
| `TestKillConsumerAfterCommit` | queda entre o commit e a remoção da mensagem |
| `TestKillPublisherHoldingClaims` | queda do publisher com eventos reservados |
| `TestGracefulTermination` | SIGTERM sob carga |
| `TestDatabaseOutage` | PostgreSQL fora do ar por 5 segundos |
| `TestBrokerOutage` | SQS e SNS fora do ar por 6 segundos |

Todo cenário termina conferindo que o saldo de cada carteira bate com o ledger, que nenhuma transação tem dois lançamentos e que cada evento chegou uma única vez à fila de auditoria. Nesses testes, a visibilidade das mensagens, o lease da outbox e os backoffs são reduzidos por variáveis de ambiente para que os cenários terminem em segundos.

Dois casos não têm teste com várias instâncias: duas reversões simultâneas da mesma aposta (a regra é garantida por um índice único no banco e testada com um processo) e a expiração de referências pendentes (testada com um processo). As três instâncias também compartilham CPU e rede, por rodarem no mesmo container. As demais limitações estão no [ARCHITECTURE.md](ARCHITECTURE.md), §7.

## Solução de problemas

| Problema | Solução |
|---|---|
| Porta 8080 ou 8180 já em uso | defina `APP_HOST_PORT` e `KEYCLOAK_HOST_PORT` no `.env` |
| `app` não fica saudável | veja `docker compose logs app`; configuração inválida aparece na partida, e dependências fora do ar aparecem em `/health/ready` |
| Arquivos criados pelo `tools` com outro dono | ajuste `HOST_UID` e `HOST_GID` no `.env` |
| `401` ou `403` nas chamadas | confira o client: carteiras usam `wallet-backoffice`, operações usam o client do próprio provedor |
| Testes de integração ou de resiliência não conectam | suba a stack antes; os de resiliência precisam também do Toxiproxy |
| Recomeçar do zero | `docker compose down -v` e depois `docker compose up --build -d --wait` |

## Estrutura do repositório

```
cmd/wallet/          ponto de entrada
internal/bootstrap/  composição Fx
internal/domain/     modelo de domínio
internal/app/        casos de uso e portas
internal/adapters/   PostgreSQL, HTTP, SQS e SNS
internal/platform/   configuração, logs, health checks e métricas
internal/worker/     publisher da outbox e worker de referências
migrations/          schema, constraints e triggers
deploy/              PostgreSQL, Keycloak, LocalStack e Toxiproxy
scripts/             testes de mutação
test/resilience/     testes com várias instâncias
compose.yaml         ambiente local e container de toolchain
Dockerfile           imagens de toolchain, build e runtime
```
