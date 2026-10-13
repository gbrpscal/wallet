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
- [Migrations](#migrations)
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
| `DATABASE_URL` | *(obrigatória)* | URL de conexão do PostgreSQL |
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
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `wallet` | Banco local |
| `KEYCLOAK_ADMIN_USER` / `KEYCLOAK_ADMIN_PASSWORD` | `admin` | Administrador local do Keycloak |

Uma configuração inválida (por exemplo, `SHUTDOWN_TIMEOUT=abc`) encerra o processo na partida, com a lista de erros.

## Filas SQS

As filas são criadas automaticamente quando o LocalStack fica pronto, pelo script [deploy/localstack/init-queues.sh](deploy/localstack/init-queues.sh):

- `wager-transactions.fifo`: entrada de operações;
- `wager-transactions-dlq.fifo`: dead-letter queue.

*Pendente (Fase 7/8):* política de redrive, visibility timeout e o destino dos eventos de saída.

## Migrations

*Pendente (Fase 4).* A ferramenta é o `goose`, já disponível no container `tools`. Os comandos de aplicação e reversão serão documentados aqui.

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

*Pendente (Fases 5 e 6):* abertura de carteira, operações, consultas, reconciliação e obtenção de token no Keycloak.

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

### Testes de mutação

O [scripts/mutation.sh](scripts/mutation.sh) comprova que os testes verificam as regras de fato. Cada mutante injeta um defeito (por exemplo, "débito deixa o saldo negativo" ou "segunda reversão aceita") numa cópia descartável do código, e algum teste precisa falhar:

```bash
docker compose run --rm tools bash scripts/mutation.sh
```

Cada linha mostra o defeito e os testes que o detectaram (`KILLED`). `SURVIVED` indica regra sem teste, e `INVALID` indica mutante que não compila. O script termina com erro se houver qualquer resultado diferente de `KILLED`.

## Testes de integração, múltiplas instâncias e falha

*Pendente (Fases 4 a 10).* Os testes são separados por build tags:

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
internal/app/          casos de uso
internal/adapters/     PostgreSQL, HTTP, SQS
internal/platform/     configuração, logs, health
deploy/                Keycloak, LocalStack, Toxiproxy
scripts/               testes de mutação
compose.yaml           stack local e container de toolchain
Dockerfile             imagens tools, build e runtime
```
