# Arquitetura

Serviço em Go que movimenta carteiras de jogadores por uma API HTTP e por um consumidor SQS FIFO, com as mesmas garantias financeiras nos dois canais, mesmo com várias instâncias em execução e falhas entre as etapas do processamento.

> **Documento incremental.** Cada fase acrescenta as decisões aprovadas. As seções marcadas como **pendente** descrevem decisões que ainda serão tomadas; nada nelas está implementado. A revisão final contra o enunciado acontece na fase de documentação.

## Sumário

1. [Visão geral](#1-visão-geral)
2. [Estado da implementação](#2-estado-da-implementação)
3. [Interpretações adotadas](#3-interpretações-adotadas)
4. [Decisões técnicas](#4-decisões-técnicas)
5. [Evidências por critério de avaliação](#5-evidências-por-critério-de-avaliação)
6. [Checklist de garantias obrigatórias](#6-checklist-de-garantias-obrigatórias)
7. [Limitações e trabalho não concluído](#7-limitações-e-trabalho-não-concluído)

---

## 1. Visão geral

### 1.1 Componentes

| Componente | Papel |
|---|---|
| API HTTP (`net/http`) | Abertura e consulta de carteiras, envio e consulta de operações, reconciliação e health checks. |
| Consumidor SQS | Lê `wager-transactions.fifo` e usa o mesmo caso de uso da API, com inbox na mesma transação SQL. |
| Worker de referências | Retoma operações em `PENDING_REFERENCE` com backoff, em paralelo por carteira. |
| Publisher da outbox | Publica, depois do commit, os eventos da outbox no tópico SNS FIFO, com reserva por lease e ordem por agregado. |
| PostgreSQL | Fonte da verdade: saldo, ledger, transações, inbox e outbox. |
| Keycloak | IdP OAuth 2.0/OIDC (`client_credentials`). |
| LocalStack | Emulador de SQS FIFO no ambiente local e nos testes. |
| Toxiproxy | Injeta indisponibilidade de PostgreSQL e SQS nos testes de falha. |

### 1.2 Organização de pacotes (hexagonal)

```
cmd/wallet/                  main: carrega a configuração e compõe o Fx
internal/domain/             modelo puro (sem Fx, HTTP, SQS ou banco)
  errors.go, ids.go, time.go   Error{Kind, Code}, IDs tipados, timestamps
  money/                       Money e Currency
  wallet/                      Wallet (raiz de agregado) e LedgerEntry
  wagering/                    WagerTransaction, Request, hash e regras
  event/                       envelope e eventos concretos
internal/app/                casos de uso (Service), portas (UnitOfWork, repositórios),
                             políticas de retry e de espera, cursor, erros de infraestrutura
internal/bootstrap/          raiz de composição: configuração e módulos Fx (main e testes)
internal/testutil/pgtest/     banco temporário para os testes de integração e de resiliência
internal/adapters/
  postgres/                  pool, UnitOfWork, repositórios e classificação de erros
  httpapi/                   servidor, rotas, autenticação OIDC, erros (problem+json), health checks
  sqs/                       cliente, consumidor e publisher
internal/platform/
  config/  logging/  health/ configuração, logs JSON e readiness
  logctx/                    atributos de log carregados pelo contexto
  metrics/                   implementação Prometheus da porta app.Metrics e servidor /metrics
migrations/                  schema, constraints e triggers (goose)
deploy/                      papéis do PostgreSQL, realm do Keycloak, filas, Toxiproxy
scripts/mutation.sh          testes de mutação curados
test/resilience/             testes com 3 processos reais, SIGKILL e Toxiproxy (tag resilience)
```

A dependência é sempre para dentro: `adapters → app → domain`. Dentro do domínio ela segue uma direção única: `wagering → wallet → money → domain`, e `event → wallet`. O teste `TestDomainHasNoInfrastructureImports` falha se o domínio importar Fx, `net/http`, AWS, pgx ou `database/sql`.

---

## 2. Estado da implementação

| Fase | Conteúdo | Estado |
|---|---|---|
| 1 | Leitura e decisões | concluída |
| 2 | Esqueleto: toolchain, Compose, módulos Fx, health checks | concluída |
| 3 | Domínio puro e testes unitários | concluída |
| 4 | Persistência: migrations, constraints, repositórios | concluída |
| 5 | Casos de uso | concluída |
| 6 | HTTP e autenticação | concluída |
| 7 | Consumidor SQS | concluída |
| 8 | Outbox e workers | concluída |
| 9 | Observabilidade | concluída |
| 10 | Testes de concorrência, multi-instância e falha | concluída |
| 11 | Documentação final | pendente |
| 12 | Verificação final | pendente |

---

## 3. Interpretações adotadas

Pontos em que o texto do enunciado tinha erro de digitação ou ambiguidade. Cada interpretação foi confirmada antes da implementação.

| # | Interpretação | Justificativa |
|---|---|---|
| 1 | O valor de abertura nos exemplos e testes é **`1000.00` BRL**. O teste obrigatório das duas apostas de 80.00 usa uma carteira de **`100.00` BRL**. | O exemplo de `POST /wallets` usa `initialBalance` 1000.00 e devolve `balance` 1000.00. Isso é coerente com a reconciliação de exemplo: abertura de 1000.00 + aposta de 25.00 = 975.00, com `checkedEntries: 2`. O cenário das duas apostas define explicitamente 100.00. |
| 2 | O `idempotencyKey` do exemplo SQS é `provider-a:transaction-123`. | `provider-a:transaction-12` era erro de digitação: o `externalTransactionId` do mesmo exemplo é `transaction-123`, e o cliente pode usar `{providerId}:{externalTransactionId}`. |
| 3 | O arquivo de módulo é **`go.mod`**, versionado com `go.sum`. | `go.md` seria um arquivo Markdown. A versão do Go fica declarada no `go.mod` (`go 1.27.2`) e no `Dockerfile` (`GO_VERSION=1.27.2`). |
| 4 | A fila é **`wager-transactions.fifo`**, com DLQ **`wager-transactions-dlq.fifo`**. | `wager-transctions.fifo` era erro de digitação. |
| 5 | O evento é **`WalletBalanceChanged`**. | `WalletBalanceChangerd` era erro de digitação. |
| 6 | "As variantes devem ser preservadas" significa **"as invariantes devem ser preservadas"**. | A frase está no bloco sobre estado encapsulado, construtores com validação e métodos de transição, que são o mecanismo para manter invariantes (saldo ≥ 0, moeda da movimentação igual à da carteira, versão incrementando só com mudança de saldo, `balanceAfter = balanceBefore ± money`). O próprio enunciado usa "invariantes" em outros pontos, e o critério de Integridade financeira também. |

---

## 4. Decisões técnicas

Todas as decisões seguem o formato: **Contexto · Opções consideradas · Prós e contras · Escolha e justificativa · Critério impactado · Como verificar · Limitações.** Os identificadores D1–D15 correspondem aos pontos de decisão do enunciado.

### 4.1 Dinheiro

#### D2. Representação de Money e tipo de coluna

- **Contexto:** "Representação: `int64` em unidades mínimas ou biblioteca decimal de precisão exata. Documentar representação e limites." / "Nunca passar por `float32`/`float64`."
- **Opções consideradas:** (a) `int64` em centavos com coluna `BIGINT`; (b) `shopspring/decimal` com `NUMERIC(20,2)`; (c) `cockroachdb/apd` com `NUMERIC`.
- **Prós e contras:**
  - (a) é exato, não tem dependência e é rápido. O overflow é tratável em toda operação. A escala fica fixa em 2, o que basta porque o enunciado exige escala fixa de duas casas.
  - (b) e (c) têm precisão arbitrária, mas trazem uma dependência para o domínio, exigem impor a escala manualmente em cada operação e são mais lentas.
- **Escolha e justificativa:** (a). O tipo `money.Money` guarda `minor int64` e `Currency`, com campos não exportados, e é imutável: toda operação devolve um novo valor. Cobre criação a partir de string decimal (`Parse`), zero por moeda (`Zero`), soma, subtração, negação, comparação e JSON.
  - **Limites:** de −92.233.720.368.547.758,08 a 92.233.720.368.547.758,07 (`math.MinInt64` e `math.MaxInt64` centavos). Valores de entrada acima do máximo são rejeitados no parsing com `INVALID_AMOUNT`.
  - **Overflow:** checado no parsing, em `Add`, `Sub` e `Neg`.
  - **Negativos:** permitidos em diferenças internas; o saldo da carteira não pode ser negativo (invariante da Wallet).
  - **Valor zero do tipo:** inválido; toda operação o rejeita. É preciso usar `Zero(currency)`.
- **Critério impactado:** Integridade financeira.
- **Como verificar:**
  - `TestParseAcceptsStrictForm`, `TestOverflow`, `TestCurrencyMismatch`, `TestZeroAndUninitialized` e `TestJSON`.
  - `TestNoFloatingPointInProductionCode` falha se `float32` ou `float64` aparecer em qualquer arquivo de produção.
  - Os mutantes de overflow e moeda do `scripts/mutation.sh`.
- **Limitações:** só moedas com 2 casas decimais. Moedas com outra escala (JPY, KWD) exigiriam escala por moeda.

#### Moedas aceitas

- **Contexto:** "Escala fixa de duas casas e código de moeda ISO 4217." / "É permitido operar só em BRL nos cenários principais, desde que o tipo carregue a moeda e existam testes de incompatibilidade entre moedas."
- **Opções consideradas:** (a) BRL, USD e EUR; (b) somente BRL; (c) qualquer código de 3 letras.
- **Prós e contras:**
  - (a) usa três moedas reais de 2 casas, o que permite testar a incompatibilidade com moedas válidas.
  - (b) só consegue testar a incompatibilidade com uma moeda que já é rejeitada na entrada, ou seja, testa outra coisa.
  - (c) aceitaria moedas com escala diferente de 2, violando a escala fixa.
- **Escolha e justificativa:** (a). `ParseCurrency` aceita exatamente `BRL`, `USD` ou `EUR`, em maiúsculas. Qualquer outro valor resulta em `UNSUPPORTED_CURRENCY`.
- **Critério impactado:** Integridade financeira.
- **Como verificar:** `TestParseCurrency`, `TestCurrencyMismatch` e `TestScopeChecksBeforeKindRules/currency_differs_from_wallet`.
- **Limitações:** adicionar uma moeda exige alterar a lista e confirmar que ela tem 2 casas decimais.

#### Formato aceito para `amount`

- **Contexto:** "Rejeitar nas entradas financeiras externas: valor vazio, `NaN`, `Infinity`, notação científica, escala excedente e valores negativos." / "Não arredondar silenciosamente entrada inválida. Se formas equivalentes forem aceitas, documentar a normalização feita antes do hash de idempotência."
- **Opções consideradas:** (a) forma estrita com exatamente duas casas; (b) forma tolerante (`"25"` e `"25.0"`) normalizada para `"25.00"`.
- **Prós e contras:**
  - (a) não exige normalização, então o hash é trivialmente determinístico.
  - (b) é mais amigável para o cliente, mas abre ambiguidade e exige normalização documentada e testada nos dois canais.
- **Escolha e justificativa:** (a). É aceita somente a expressão `^(0|[1-9][0-9]*)\.[0-9]{2}$`. São rejeitados: sinal, expoente, espaços, zeros à esquerda (`01.00`), vírgula, dígitos não ASCII e qualquer escala diferente de 2. Valor numérico JSON (`"amount": 25.00` sem aspas) também é rejeitado, então nunca passa por ponto flutuante. Nada é arredondado.
- **Critério impactado:** Integridade financeira e Idempotência.
- **Como verificar:** `TestParseRejectsInvalidAmounts` (28 entradas inválidas) e `TestJSON`.
- **Limitações:** clientes que enviam `"25"` precisam corrigir a entrada; o erro é corrigível (`INVALID_AMOUNT`).

### 4.2 Identificadores

#### Identificadores internos: UUIDv7 com `google/uuid`

- **Contexto:** os exemplos do enunciado usam UUIDv7 (`0192f291-27dd-7d3f-…`). O cursor do ledger precisa de "ordenação estável".
- **Opções consideradas:** (a) UUIDv7 com `github.com/google/uuid`; (b) UUIDv7 com `gofrs/uuid`; (c) implementação própria.
- **Prós e contras:**
  - (a) e (b) são equivalentes; (a) é a biblioteca mais difundida e não tem dependências.
  - (c) não tem dependência, mas exige manter e testar código da RFC 9562 sem ganho real.
- **Escolha e justificativa:** (a). O domínio usa um tipo genérico `ID[T]` com instâncias distintas (`WalletID`, `PlayerID`, `TransactionID`, `LedgerEntryID`, `EventID`). Assim, um ID não pode ser trocado por outro em tempo de compilação, e a biblioteca não aparece na API do domínio.
  - `ParseID` aceita só a forma canônica de 36 caracteres; maiúsculas são normalizadas para minúsculas.
  - URN, chaves, a forma sem hífens e o UUID nulo são rejeitados.
  - Os IDs são gerados no próprio domínio.
- **Critério impactado:** Modelagem e arquitetura.
- **Como verificar:** `TestParseID`.
- **Limitações:** UUIDv7 expõe o instante aproximado de criação do registro.

#### Identificadores textuais do provedor

- **Contexto:** `providerId`, `externalTransactionId`, `idempotencyKey`, `roundId` e `gameId` são strings livres; o enunciado não define limites.
- **Opções consideradas:** (a) de 1 a 255 bytes, UTF-8 válido; (b) de 1 a 128 bytes; (c) sem limite.
- **Prós e contras:**
  - (a) protege índices e logs e comporta IDs longos.
  - (b) é mais restritiva e pode recusar IDs de alguns provedores.
  - (c) deixa entradas gigantes chegarem ao banco e aos logs.
- **Escolha e justificativa:** (a). O valor precisa ser não vazio, ter até 255 bytes, ser UTF-8 válido e não ter caracteres de controle. Espaço nas pontas é **rejeitado, não removido**, para não haver normalização silenciosa antes do hash.
- **Critério impactado:** Idempotência e Modelagem.
- **Como verificar:** `TestValidateTextID` e `TestRequestFieldValidation`.
- **Limitações:** o limite é em bytes, então caracteres multibyte reduzem o número de caracteres aceitos.

### 4.3 Modelo de domínio

#### Erros de domínio: tipo único com Kind e Code

- **Contexto:** "Erros de domínio classificáveis por tipo ou `errors.Is`/`errors.As`. `panic` não representa rejeição de negócio."
- **Opções consideradas:** (a) `*domain.Error` com `Kind` e `Code`, mais um sentinel por código; (b) apenas sentinels; (c) um struct por categoria.
- **Prós e contras:**
  - (a) deixa HTTP e SQS mapearem pelo `Kind` sem conhecer cada código.
  - (b) espalha a classificação corrigível vs. definitivo pelos adaptadores.
  - (c) é equivalente a (a), mas com mais tipos para manter.
- **Escolha e justificativa:** (a).
  - `Kind` assume `Validation`, `Conflict`, `Rejection` ou `Invariant`.
  - `errors.Is` compara pelo código, então funciona com erros encapsulados que levam mensagem específica (`domain.Errorf`).
  - `errors.As` dá acesso ao `Kind` e à mensagem.
  - Nenhum `panic` em caminhos de negócio. A única exceção é `MustParseID`, usado só em testes.
- **Critério impactado:** Modelagem e arquitetura.
- **Como verificar:** `TestErrorMatchingByCode`.
- **Limitações:** um código novo exige registrar o sentinel na tabela `byCode`.

#### Produção de lançamentos e eventos: retorno explícito

- **Contexto:** "Criação e reidratação separadas. A reidratação não reaplica movimentações, transições nem emissão de eventos." / "Tipo e versão definidos pelo construtor do evento."
- **Opções consideradas:** (a) os métodos devolvem o resultado (lançamento, eventos) de forma explícita; (b) o agregado acumula eventos internamente, lidos com `PullEvents()`.
- **Prós e contras:**
  - (a) não tem estado oculto, e a reidratação naturalmente não emite nada.
  - (b) é o padrão DDD clássico, mas cria estado oculto, com risco de esquecer o `Pull` ou de duplicar eventos ao reprocessar.
- **Escolha e justificativa:** (a).
  - `Wallet.Debit` e `Wallet.Credit` devolvem o `LedgerEntry`.
  - `wagering.Process`, `wagering.Expire` e `wagering.OpenWallet` devolvem um `Outcome` com transação, lançamento e eventos, que o caso de uso persiste em um único commit.
  - O instante `now` entra como parâmetro, o que deixa os testes determinísticos.
- **Critério impactado:** Modelagem e arquitetura e Integridade financeira.
- **Como verificar:** `TestRehydrateDoesNotApplyMovements`, `TestRehydrateRoundTrip` e `TestOpenWalletPositive`.
- **Limitações:** quem chama precisa persistir todos os itens do `Outcome` juntos; isso será garantido pela delimitação de transação (D11).

#### Organização em subpacotes por conceito

- **Contexto:** "O domínio permanece independente de Fx, HTTP, SQS e bibliotecas de persistência." / "Entidades com estado encapsulado."
- **Opções consideradas:** (a) subpacotes `money`, `wallet`, `wagering` e `event`; (b) um único pacote `domain`.
- **Prós e contras:**
  - (a) dá encapsulamento real: um pacote não enxerga os campos não exportados de outro.
  - (b) é mais simples, mas o encapsulamento entre agregados fica só por convenção.
- **Escolha e justificativa:** (a). `Wallet` e `LedgerEntry` ficam juntos porque saldo e lançamento nascem na mesma operação.
- **Critério impactado:** Modelagem e arquitetura.
- **Como verificar:** compilação, e `TestDomainHasNoInfrastructureImports`.
- **Limitações:** nenhuma relevante.

#### Timestamps

- **Contexto:** "Timestamps UTC em RFC 3339." O PostgreSQL armazena microssegundos.
- **Opções consideradas:** (a) normalizar para UTC com precisão de microssegundos no domínio; (b) manter nanossegundos e o fuso de origem.
- **Prós e contras:**
  - (a) faz o valor em memória ser igual ao persistido.
  - (b) faz o valor reidratado diferir do original.
- **Escolha e justificativa:** (a), com `domain.Timestamp`. Os eventos usam o layout `2006-01-02T15:04:05.000000Z07:00` (RFC 3339 em UTC).
- **Critério impactado:** Integridade financeira (replay devolve exatamente o que foi persistido).
- **Como verificar:** `TestTimestampsUseUTCMicroseconds` e `TestWalletBalanceChangedEnvelope`.
- **Limitações:** nenhuma.

### 4.4 Wallet

- **Invariantes garantidas pelo agregado:**
  - saldo ≥ 0;
  - moeda da movimentação igual à da carteira;
  - valor de movimentação > 0;
  - versão inicial 1, incrementando só quando o saldo muda;
  - `updatedAt` monotônico.
- Em caso de rejeição, a carteira fica inalterada: o lançamento é construído antes de qualquer mutação.
- `LedgerEntry` é imutável. O construtor exige `balanceAfter = balanceBefore ± amount`, valor positivo, saldos não negativos e moedas iguais.
- Além dos campos mínimos do enunciado, o lançamento grava **`walletVersion`**: a versão de carteira que ele produziu (1 na abertura com saldo; a versão anterior + 1 nas movimentações). Ela encadeia os lançamentos de uma carteira, permite ao banco conferir saldo × ledger (§4.11) e dá a ordenação estável do ledger.
- As mesmas regras são garantidas também pelo banco (§4.11).

**Como verificar:** `TestDebitAndCredit`, `TestInsufficientFundsLeavesWalletUnchanged`, `TestMovementValidation`, `TestRehydrateRejectsInconsistentState` e `TestLedgerEntryInvariant`.

### 4.5 D7. Máquina de estados e falhas transitórias vs. permanentes

- **Contexto:** "Estado inicial `PENDING`. Transições para processamento, espera de referência, rejeição e falha permanente validadas pelo domínio." / "Documentar a máquina de estados e como se distinguem falhas transitórias de permanentes."
- **Opções consideradas (estados):** (a) os 5 estados do enunciado com transições diretas; (b) o mesmo, mais um retorno de `PENDING_REFERENCE` para `PENDING` quando a referência aparece.
- **Opções consideradas (FAILED):** (c) só para inconsistência irrecuperável; (d) também quando os retries transitórios se esgotam.
- **Prós e contras:**
  - (a) é exatamente a tabela do enunciado.
  - (b) acrescenta um passo e um estado intermediário a recuperar após uma queda, sem ganho.
  - (c) não transforma uma indisponibilidade longa em resultado definitivo.
  - (d) entregaria ao provedor uma falha que um retry posterior resolveria.
- **Escolha e justificativa:** (a) + (c).

```
PENDING           -> PROCESSED | REJECTED | FAILED | PENDING_REFERENCE
PENDING_REFERENCE -> PROCESSED | REJECTED | FAILED
PROCESSED, REJECTED, FAILED: terminais (nenhuma transição)
```

  - Só reversões (`REFUND`, `ROLLBACK`) podem ir para `PENDING_REFERENCE`.
  - **Falha transitória:** erro de conexão, deadlock, serialização, timeout ou SQS indisponível. Ela **nunca muda o estado**: a transação SQL é desfeita e o retry fica com o chamador, o worker ou o SQS.
  - **Falha permanente (`FAILED`, código `INCONSISTENT_STATE`):** dado persistido que viola invariante na reidratação. Registrada para auditoria, sem evento (nenhum dos quatro eventos do enunciado corresponde a ela).
  - Uma transição inválida resulta em `INVALID_STATE_TRANSITION`, código interno que indica bug e nunca é persistido como `failureCode`.
- **Critério impactado:** Integridade financeira e Mensageria e recuperação.
- **Como verificar:** `TestStateMachine` (todas as 25 combinações de origem e destino), `TestTerminalTransactionsRejectTransitions`, `TestOnlyReversalsWaitForReference` e `TestMarkValidatesCodes`.
- **Limitações:** a classificação dos erros do PostgreSQL está no §4.11; a dos erros do SDK da AWS será feita na Fase 7.

### 4.6 WagerTransaction

- **Contexto:** "Para operações externas, registrar identificadores interno e externo, provedor, chave de idempotência, hash do payload, carteira, jogador, rodada, jogo, tipo, Money, referência externa opcional, estado e timestamps. Quando aplicável: referência interna resolvida, código de falha e resultado financeiro retornado ao provedor."
- **Implementação:**
  - Construção por `NewExternal` (a partir de um `Request` validado) ou pela abertura interna. Reconstrução por `Rehydrate`, que valida a consistência completa do snapshot.
  - O `OPENING` não carrega provedor, ID externo, chave, hash, rodada, jogo nem referência; `Rehydrate` rejeita um `OPENING` com qualquer um desses campos.
  - **Resultado financeiro devolvido no replay:** `PROCESSED` grava o saldo observado logo após o processamento. `REJECTED` grava o saldo observado no momento da rejeição, exceto em `CURRENCY_MISMATCH`, em que o saldo está em outra moeda.
- **Como verificar:** `TestNewExternalStartsPending`, `TestRehydrateRoundTrip`, `TestRehydrateRejectsInconsistentState` e `TestOpenWalletPositive`.

### 4.7 Idempotência

#### D3. Hash de idempotência

- **Contexto:** "Hash determinístico dos campos de negócio com JSON canônico e chaves ordenadas, excluindo a chave de idempotência e metadados de transporte. Documentar algoritmo, campos e normalizações, garantindo equivalência entre HTTP e SQS."
- **Opções consideradas:** (a) SHA-256 de JSON canônico com prefixo de versão; (b) o mesmo sem prefixo; (c) HMAC-SHA256 com segredo.
- **Prós e contras:**
  - (a) permite evoluir o conjunto de campos sem ambiguidade com hashes já persistidos.
  - (b) é mais simples, mas uma mudança futura invalida os hashes sem como distingui-los.
  - (c) exige gestão de segredo sem ganho, porque o payload já está no banco.
- **Escolha e justificativa:** (a). O hash é calculado uma única vez, em `wagering.NewRequest`, e HTTP e SQS montam o mesmo `RequestInput`.
  - **Algoritmo:** `"v1:" + hex(SHA-256(json))`.
  - **Campos:** `externalTransactionId`, `gameId`, `kind`, `money.amount`, `money.currency`, `playerId`, `providerId`, `roundId` e `walletId`. `referenceExternalTransactionId` entra só quando presente.
  - **Fora do hash:** `idempotencyKey`, `messageId`, `occurredAt`, `type` do envelope SQS e cabeçalhos HTTP.
  - **JSON canônico:** chaves ordenadas, sem espaços, UTF-8 e sem escape de HTML (`<`, `>` e `&` literais).
  - **Normalizações:** a única é a de UUIDs (`playerId` e `walletId`), convertidos para a forma canônica minúscula. Strings não passam por trim nem por normalização Unicode, e `amount` já chega na forma estrita.
- **Critério impactado:** Idempotência.
- **Como verificar:**
  - `TestHashCanonicalVector` fixa o vetor esperado, calculado independentemente sobre a string canônica.
  - `TestHashExcludesKeyAndNormalizesUUIDCase`, `TestHashChangesWithEveryBusinessField` e `TestHashHandlesHTMLCharactersVerbatim`.
  - Os mutantes de hash do `scripts/mutation.sh`.
- **Limitações:** acrescentar um campo exige uma nova versão (`v2:`) e convivência com os hashes `v1:`.

#### Escopo de unicidade da Idempotency-Key

- **Contexto:** "Isolamento entre provedores, inclusive em consultas e replays." / "Operação `(providerId, externalTransactionId)` não pode ser reaplicada com outra chave."
- **Opções consideradas:** (a) chave única por provedor `(providerId, idempotencyKey)`; (b) chave global.
- **Prós e contras:**
  - (a) impede que um provedor colida com outro e que receba a informação de que a chave de outro provedor existe.
  - (b) faz um provedor receber conflito por causa da chave de outro, vazando informação.
- **Escolha e justificativa:** (a). As buscas de replay são sempre por `(providerId, idempotencyKey)` ou `(providerId, externalTransactionId)`. `CheckReplay` devolve:
  - `nil` para a mesma chave com o mesmo hash (replay);
  - `IDEMPOTENCY_KEY_CONFLICT` para a mesma chave com outro hash;
  - `EXTERNAL_TRANSACTION_CONFLICT` para a mesma operação com outra chave;
  - `INCONSISTENT_STATE` se uma busca cruzar provedores.
- **Critério impactado:** Idempotência e Modelagem (autorização).
- **Como verificar:** `TestCheckReplay` no domínio; no banco, os índices `wager_transactions_provider_key_uq` e `wager_transactions_provider_external_uq` (`TestIdempotencyConstraints`, `TestBetRoundTripAndLookups`, que confirma que outro provedor não enxerga a operação).
- **Limitações:** o fluxo completo de replay (reler e devolver o resultado persistido) é implementado nos casos de uso (Fase 5).

### 4.8 Operações, referências e reversões

#### Regras por tipo

| Tipo | Movimentação | Valor | Referência |
|---|---|---|---|
| `BET` | débito; sem saldo resulta em `INSUFFICIENT_FUNDS` | > 0 | proibida (`INVALID_REQUEST`) |
| `WIN` | crédito | > 0 | opcional (ver abaixo) |
| `LOSS` | nenhuma; não cria ledger nem altera a versão; emite só `WagerTransactionProcessed` | `0.00` | proibida (`INVALID_REQUEST`) |
| `REFUND` | crédito; devolve integralmente uma `BET` | > 0, igual ao referenciado | obrigatória (`REFERENCE_REQUIRED`) |
| `ROLLBACK` | contrária à original: crédito para `BET`, débito para `WIN` ou `REFUND` | > 0, igual ao referenciado | obrigatória (`REFERENCE_REQUIRED`) |

Também valem estas regras:
- Uma operação que referencia a si mesma resulta em `INVALID_REQUEST`.
- `OPENING`, ou qualquer tipo desconhecido, enviado por HTTP ou SQS resulta em `KIND_NOT_ALLOWED`.
- Antes das regras do tipo, a carteira precisa pertencer ao jogador (`WALLET_PLAYER_MISMATCH`) e ter a mesma moeda da operação (`CURRENCY_MISMATCH`).

#### Referência em `BET` e `LOSS`

- **Contexto:** o enunciado só define referência para `WIN`, `REFUND` e `ROLLBACK`.
- **Opções consideradas:** (a) rejeitar com `INVALID_REQUEST`; (b) aceitar e só armazenar.
- **Prós e contras:**
  - (a) evita aceitar um campo sem semântica, que entraria no hash.
  - (b) guarda um dado sem regra.
- **Escolha e justificativa:** (a).
- **Critério impactado:** Integridade financeira e Idempotência.
- **Como verificar:** `TestRequestReferences`.
- **Limitações:** nenhuma.

#### Referência opcional do `WIN`

- **Contexto:** "`WIN`: valor positivo; pode referenciar uma aposta da mesma rodada."
- **Opções consideradas:** (a) validar se existir e, se não existir, creditar normalmente; (b) usar o mesmo fluxo de espera das reversões; (c) ignorar a referência.
- **Prós e contras:**
  - (a) não atrasa o prêmio, porque o crédito não depende da aposta.
  - (b) atrasa prêmios por uma dependência que o enunciado não exige.
  - (c) permitiria um `WIN` apontando para aposta de outra rodada ou de outro jogador.
- **Escolha e justificativa:** (a).
  - Referência ausente: o crédito acontece e a referência fica só registrada.
  - Referência presente mas que não é `BET`, ou que diverge em provedor, jogador, carteira, moeda ou rodada: `REFERENCE_MISMATCH`.
  - Referência presente mas não `PROCESSED`: `REFERENCE_NOT_PROCESSED`.
- **Critério impactado:** Integridade financeira.
- **Como verificar:** `TestWin` (6 subcasos).
- **Limitações:** um `WIN` processado antes da sua `BET` não fica com a referência resolvida.

#### D5. `REFUND` + `ROLLBACK` sobre a mesma aposta

- **Contexto:** "Uma referência não pode receber duas reversões bem-sucedidas do mesmo tipo." / "Combinação de `REFUND` e `ROLLBACK` sobre a mesma aposta: tratamento a definir e documentar, impedindo devolução duplicada do mesmo débito."
- **Opções consideradas:** (a) no máximo uma reversão `PROCESSED` por transação referenciada, de qualquer tipo; (b) a segunda reversão vira `PROCESSED` sem movimentação.
- **Prós e contras:**
  - (a) é inequívoca para auditoria e impede devolução dupla.
  - (b) é mais tolerante, mas um "sucesso sem efeito" é ambíguo e conta como duas reversões bem-sucedidas.
- **Escolha e justificativa:** (a). A segunda reversão é `REJECTED` com `ALREADY_REVERSED`, sem movimentação. Um `ROLLBACK` de um `REFUND` continua permitido, porque referencia outra transação. O índice único `wager_transactions_one_reversal_uq` impõe a regra também no banco.
- **Critério impactado:** Integridade financeira.
- **Como verificar:** `TestReversalRejections/already_reversed` (domínio) e `TestOneSuccessfulReversalPerReference` (banco). Entre processos, a regra é garantida pelo índice único no banco; não há um teste multi-instância dedicado a duas reversões simultâneas da mesma aposta (§7).
- **Limitações:** depois de `BET` → `REFUND` → `ROLLBACK do REFUND`, a `BET` não pode ser devolvida de novo (a regra é uma reversão por referência).

#### D5. Referência existente mas não processada

- **Contexto:** "Explicar o comportamento quando a referência existe mas está pendente ou terminou sem sucesso."
- **Opções consideradas:** (a) referência pendente faz esperar e referência terminal sem sucesso rejeita; (b) tudo espera até o TTL; (c) tudo rejeita imediatamente.
- **Prós e contras:**
  - (a) espera só quando ainda há chance de conclusão.
  - (b) retarda uma resposta que já é definitiva.
  - (c) rejeitaria indevidamente a reversão de uma `BET` ainda em processamento.
- **Escolha e justificativa:** (a).
  - Referência ausente, `PENDING` ou `PENDING_REFERENCE`: a reversão vai para `PENDING_REFERENCE` e emite `WagerTransactionPendingReference`.
  - Referência `REJECTED` ou `FAILED`: `REJECTED` imediato com `REFERENCE_NOT_PROCESSED`, porque a operação nunca movimentou saldo.
  - Ordem das checagens: primeiro as estáticas (tipo reversível, escopo, valor), que rejeitam sem esperar; depois o estado da referência; por fim a reversão prévia.
  - Ao esgotar a espera, a reversão é finalizada como `REJECTED` com `REFERENCE_NOT_FOUND`, mesmo que a referência exista mas tenha ficado pendente até o fim.
- **Critério impactado:** Integridade financeira e Mensageria e recuperação.
- **Como verificar:** `TestReversalBeforeReference`, `TestReversalOfRejectedReference` e `TestExpire`.
- **Limitações:** os parâmetros de espera estão na D4 (§4.12).

#### Reversão sem saldo

- **Contexto:** "Reversão que precisaria debitar mais que o saldo disponível é rejeitada e auditável, com `failureCode` diferente do usado em aposta sem saldo."
- **Escolha:** `REVERSAL_INSUFFICIENT_FUNDS` (diferente de `INSUFFICIENT_FUNDS`), com a transação persistida como `REJECTED`.
- **Como verificar:** `TestReversalWithoutFundsUsesDistinctCode` e `TestBetAndReversalInsufficientFundsCodesDiffer`.

### 4.9 D6. Catálogo de `failureCode`

| Classe | Código | Significado | Persiste? |
|---|---|---|---|
| Corrigível | `INVALID_REQUEST` | campo ausente ou malformado | não |
| Corrigível | `INVALID_AMOUNT` | vazio, NaN, Infinity, notação científica, escala, negativo, overflow | não |
| Corrigível | `UNSUPPORTED_CURRENCY` | moeda fora de BRL, USD e EUR | não |
| Corrigível | `AMOUNT_MUST_BE_POSITIVE` | `BET`, `WIN`, `REFUND` ou `ROLLBACK` com `0.00` | não |
| Corrigível | `LOSS_AMOUNT_MUST_BE_ZERO` | `LOSS` com valor diferente de `0.00` | não |
| Corrigível | `KIND_NOT_ALLOWED` | `OPENING` ou tipo desconhecido via HTTP/SQS | não |
| Corrigível | `REFERENCE_REQUIRED` | `REFUND`/`ROLLBACK` sem referência | não |
| Conflito | `IDEMPOTENCY_KEY_CONFLICT` | mesma chave, payload diferente | não |
| Conflito | `EXTERNAL_TRANSACTION_CONFLICT` | mesma operação com outra chave | não |
| Conflito | `WALLET_ALREADY_EXISTS` | mesmo `(playerId, currency)` | não |
| Não encontrado | `WALLET_NOT_FOUND` | operação, consulta ou reconciliação de carteira inexistente | não |
| Não encontrado | `TRANSACTION_NOT_FOUND` | consulta de transação inexistente (ou de outro provedor) | não |
| Definitivo | `INSUFFICIENT_FUNDS` | `BET` sem saldo | `REJECTED` |
| Definitivo | `REVERSAL_INSUFFICIENT_FUNDS` | reversão que debitaria além do saldo | `REJECTED` |
| Definitivo | `CURRENCY_MISMATCH` | moeda diferente da moeda da carteira | `REJECTED` |
| Definitivo | `WALLET_PLAYER_MISMATCH` | carteira não pertence ao jogador | `REJECTED` |
| Definitivo | `REFERENCE_NOT_FOUND` | espera pela referência esgotada | `REJECTED` |
| Definitivo | `REFERENCE_NOT_PROCESSED` | referência `REJECTED`/`FAILED` (ou não processada, no `WIN`) | `REJECTED` |
| Definitivo | `REFERENCE_MISMATCH` | provedor, jogador, carteira, moeda, rodada ou tipo divergentes | `REJECTED` |
| Definitivo | `REFERENCE_AMOUNT_MISMATCH` | valor diferente do valor referenciado | `REJECTED` |
| Definitivo | `REFERENCE_KIND_NOT_REVERSIBLE` | ex.: `REFUND` de `WIN`, `ROLLBACK` de `ROLLBACK` ou `LOSS` | `REJECTED` |
| Definitivo | `ALREADY_REVERSED` | referência já revertida | `REJECTED` |
| Permanente | `INCONSISTENT_STATE` | dado persistido viola invariante | `FAILED` |
| Interno | `INVALID_STATE_TRANSITION` | bug de programação; nunca persistido | não |

- **Contexto:** "Toda rejeição tem `failureCode` estável e documentado, distinguindo entradas corrigíveis de resultados definitivos."
- **Escolha:** a classe de cada código é fixada pelo seu sentinel. `MarkRejected` só aceita códigos da classe *Definitivo*, e `MarkFailed` só aceita os da classe *Permanente*.
- **Como verificar:** `TestMarkValidatesCodes`, `TestReversalRejections`, `TestRequestZeroAmountPolicy` e `TestRequestReferences`.
- **Pendente:** o mapeamento para códigos HTTP e corpos de resposta, que é a parte da D6 decidida na Fase 5/6.

### 4.10 Eventos

- **Contexto:** "Tipos concretos por evento. Envelope: `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` (opcional), `occurredAt`, `version`, `data` tipado." / "Payload da outbox é snapshot imutável."
- **Implementação:**
  - Quatro tipos concretos: `WagerTransactionProcessed`, `WagerTransactionRejected`, `WalletBalanceChanged` e `WagerTransactionPendingReference`.
  - Tipo e versão (1) são fixados pelo construtor. `eventId` é UUIDv7, gerado uma única vez na criação.
  - `aggregateId` é o `transactionId` nos eventos de transação e o `walletId` em `WalletBalanceChanged`.
  - `correlationId` é obrigatório; `causationId` é opcional e omitido quando vazio.
  - Valores monetários saem como strings decimais (`{"amount":"25.00","currency":"BRL"}`).
  - `WalletBalanceChanged.data` contém `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter` e `walletVersion`.
  - Os eventos do `OPENING` omitem os metadados externos.
  - `event.Marshal` produz os bytes armazenados na outbox, que são republicados sem alteração.
- **Como verificar:** `TestWalletBalanceChangedEnvelope`, `TestEventRequiresCorrelation`, `TestCausationIsOptional` e `TestLossHasNoMovement` (sem `WalletBalanceChanged`).
- **Pendente:** destino dos eventos (D10) e publisher (Fase 8).

### 4.11 Persistência, transações e concorrência

#### D1. Concorrência por carteira: `FOR UPDATE` + versão no UPDATE

- **Contexto:** "Coordenação por carteira. Locks globais são proibidos; carteiras independentes avançam em paralelo." / "Estratégia: lock pessimista, otimista com retry limitado, atualização atômica condicionada ou combinação justificada." / "Atualizações de saldo impedem lost updates."
- **Opções consideradas:** (a) lock pessimista na linha da carteira (`SELECT … FOR UPDATE`) com a versão conferida no UPDATE; (b) UPDATE atômico condicionado (`balance = balance - $1 WHERE balance >= $1`); (c) otimista por versão com retry limitado.
- **Prós e contras:**
  - (a) serializa só escritores da mesma carteira e deixa o domínio ver o saldo exato antes de decidir, o que dá rejeições limpas e auditáveis. Custo: escritores da mesma carteira entram em fila.
  - (b) usa o mínimo de round-trips, mas o domínio não enxerga o saldo antes de decidir, e reversões (que leem a referência e escolhem a direção) ficam difíceis de manter consistentes.
  - (c) não segura lock, mas sob contenção (50 apostas iguais, 2×80 em 100) gera muitos retries e pode esgotá-los, devolvendo erro ao cliente.
- **Escolha e justificativa:** (a), uma combinação de pessimista com checagem otimista.
  - `WalletRepository.Lock` faz `SELECT … FOR UPDATE` só na linha da carteira.
  - `WalletRepository.Save` grava com `WHERE id = $1 AND version = $esperada`. Sob o lock essa condição nunca falha; se falhar (uma escrita sem lock), o resultado é `ErrConcurrentUpdate` e nada é confirmado.
  - **Ordem de locks:** carteira primeiro. Operação e referência sempre pertencem à mesma carteira (a FK composta da referência garante isso), então duas reversões da mesma aposta são serializadas pelo mesmo lock, sem deadlock.
  - **Nível de isolamento:** `READ COMMITTED`. A serialização vem do lock de linha, não do nível de isolamento.
  - **`lock_timeout`:** `DB_LOCK_TIMEOUT`, padrão 5 s, aplicado por transação com `set_config(..., true)`. Uma espera maior resulta em `55P03`, classificado como transitório.
  - As invariantes não dependem do lock: o banco as impõe sozinho (constraints e triggers abaixo).
- **Critério impactado:** Concorrência e Integridade financeira.
- **Como verificar:**
  - `TestConcurrentDebitsDoNotLoseUpdates` (40 apostas paralelas na mesma carteira; saldo, versão e cadeia exatos).
  - `TestTwoConcurrentBetsOf80On100`, `TestIndependentWalletsDoNotBlock`, `TestSaveDetectsStaleVersion` e `TestLockTimeoutIsTransient`.
  - Os mutantes "lock da carteira sem FOR UPDATE" e "UPDATE sem checar a versão".
- **Entre processos:** `TestBurstOnOneWalletAcrossInstances` dispara 60 apostas distintas na mesma carteira pelos 3 processos e exige **zero** conflitos de concorrência: o lock serializa os escritores de processos diferentes, e a checagem de versão nunca precisa agir (§4.19).
- **Limitações:** uma carteira muito disputada tem o throughput limitado à serialização dos seus escritores.

#### D11. Acesso ao banco: pgx puro com UnitOfWork

- **Contexto:** "`pgx` com SQL explícito é preferencial. […] Transações, locks e constraints devem permanecer explícitos e verificáveis." / "Onde a transação SQL é delimitada entre repositórios."
- **Opções consideradas:** (a) pgx puro, com a transação entregue pela porta `UnitOfWork`; (b) pgx + sqlc; (c) pgx com a transação viajando no `context.Context`.
- **Prós e contras:**
  - (a) deixa SQL, locks e commit visíveis num único ponto.
  - (b) dá tipagem gerada, mas acrescenta uma ferramenta, código gerado e mapeamento extra para o domínio.
  - (c) tem menos parâmetros, mas cria uma dependência implícita: um repositório chamado sem a tx no contexto roda fora da transação sem aviso.
- **Escolha e justificativa:** (a).
  - A porta `app.UnitOfWork.Do(ctx, func(ctx, app.Tx) error)` abre uma `pgx.Tx`, entrega os repositórios ligados a ela e faz commit só se a função não devolver erro. Qualquer erro, ou a falha das conferências diferidas no commit, desfaz tudo.
  - O rollback usa um contexto próprio, para ocorrer mesmo com o contexto da requisição cancelado.
  - Os repositórios só existem dentro de uma `Tx`; não há como gravar fora da transação.
- **Critério impactado:** Integridade financeira e Modelagem e arquitetura.
- **Como verificar:** `TestUnitOfWorkRollsBackEverything` e `TestOpeningRoundTrip`.
- **Limitações:** a classificação de erro depende de os repositórios traduzirem os erros do pgx (`errors.go`).

#### Classificação de erros do banco (complemento da D7)

`postgres.translate` converte erros do pgx para a camada de aplicação:

| Origem | Classificação |
|---|---|
| `40001`, `40P01`, `55P03`, `57014`, `57P01`–`57P03`, classes `08` e `53`, erros de rede ou conexão, `context.DeadlineExceeded` | `app.ErrTransient` (nada confirmado; pode repetir) |
| `23505` em `wallets_player_currency_key` | `WALLET_ALREADY_EXISTS` |
| outros `23505` | `app.DuplicateError{Constraint}`; o caso de uso decide (ex.: reler e devolver o replay) |
| outros `23xxx` (CHECK, FK, triggers) | `INCONSISTENT_STATE`: o código tentou gravar algo que viola uma invariante |
| `pgx.ErrNoRows` | `app.ErrNotFound` |

`app.IsRetryable` é verdadeiro para `ErrTransient` e `ErrConcurrentUpdate`.

**Como verificar:** `TestLockTimeoutIsTransient`, `TestIdempotencyConstraints`, `TestOneWalletPerPlayerAndCurrency` e o mutante "lock_timeout não classificado como transitório".

#### Aplicação das migrations: serviço one-shot no Compose

- **Contexto:** "Aplicação e reversão das migrations" (README) / "Execução reproduzível a partir de checkout limpo".
- **Opções consideradas:** (a) serviço `migrate` no Compose, que roda `goose up` e termina; (b) migrations aplicadas na partida da aplicação; (c) somente manual.
- **Prós e contras:**
  - (a) não exige privilégio de DDL na aplicação, e várias instâncias não disputam a migration.
  - (b) exige DDL na aplicação e um lock para que 3 ou mais instâncias não apliquem ao mesmo tempo.
  - (c) quebra o `docker compose up --build` a partir de checkout limpo.
- **Escolha e justificativa:** (a). A aplicação depende de `migrate` com `service_completed_successfully`, e a reversão é feita com `goose down` ou `goose down-to 0` pelo container `tools`.
- **Como verificar:** `docker compose up --build` a partir de volume vazio (o `migrate` termina com código 0 e a readiness responde ok) e `TestMigrationsApplyRevertAndReapply`.
- **Limitações:** as migrations são escritas para avançar; dados gravados não sobrevivem a um `down` das tabelas.

#### Proteção do ledger: triggers + papéis separados

- **Contexto:** "No banco: unicidade de `(walletId, transactionId)` e proteção contra edição ou exclusão." / "Unicidade, não negatividade e imutabilidade do ledger impostas por schema, constraints e mecanismos de proteção do banco."
- **Opções consideradas:** (a) triggers e papéis separados; (b) somente triggers; (c) somente privilégios.
- **Prós e contras:**
  - (a) cria duas barreiras independentes.
  - (b) ainda deixa o dono do schema, que também seria a aplicação, desabilitar o trigger.
  - (c) não protege contra o dono do schema.
- **Escolha e justificativa:** (a).
  - `wallet_owner` é o dono do schema e só é usado pelas migrations.
  - `wallet_app` é o papel da aplicação: tem só `SELECT` e `INSERT` no ledger, e nenhum `DELETE` em tabela alguma.
  - Triggers `BEFORE UPDATE/DELETE/TRUNCATE` barram alterações no ledger também para o dono.
  - Carteiras, transações e eventos da outbox também não podem ser apagados.
- **Critério impactado:** Integridade financeira.
- **Como verificar:** `TestLedgerIsAppendOnly` (privilégio negado para `wallet_app`; trigger para `wallet_owner`), `TestWalletsAndTransactionsCannotBeDeleted` e os mutantes "aplicação pode alterar e apagar o ledger" e "dono do schema pode apagar lançamento".
- **Limitações:** um superusuário ou o dono do schema ainda pode desabilitar triggers deliberadamente. A proteção é contra erro e uso indevido da aplicação, não contra um administrador mal-intencionado.

#### Saldo × ledger: versão no lançamento + conferência diferida

- **Contexto:** "Toda mudança financeira exige o lançamento correspondente no ledger, confirmado junto com o saldo." / "Invariantes financeiras garantidas no banco, independentemente de locks locais."
- **Opções consideradas:** (a) `wallet_version` no lançamento, `UNIQUE (wallet_id, wallet_version)` e constraint trigger diferido; (b) só aplicação (UnitOfWork) mais reconciliação posterior.
- **Prós e contras:**
  - (a) faz o banco recusar no commit um saldo sem o lançamento correspondente.
  - (b) é mais simples, mas um bug ou um UPDATE manual cria uma divergência que o banco aceita e que só a reconciliação detecta depois.
- **Escolha e justificativa:** (a), em três camadas:
  1. **CHECKs do lançamento:** `balance_after = balance_before ± amount`, valor > 0, saldos ≥ 0. A moeda do lançamento é a da carteira e a da transação (FKs compostas), e o lançamento pertence à carteira da transação.
  2. **Trigger `BEFORE INSERT` de cadeia:** o primeiro lançamento parte de saldo 0 (versão 1, ou 2 se a carteira foi aberta com zero); cada seguinte continua exatamente do anterior. O valor é o da transação, `LOSS` nunca gera lançamento, e a direção é coerente com o tipo (`BET` debita; `OPENING`, `WIN` e `REFUND` creditam).
  3. **Constraint trigger `DEFERRABLE INITIALLY DEFERRED`** em `wallets` e no ledger: no COMMIT, o saldo e a versão de cada carteira tocada são os do seu último lançamento (ou zero na versão 1, sem lançamento). O trigger lê o estado atual da linha, porque a carteira pode mudar várias vezes na mesma transação.
  
  Em `wallets`, um trigger exige versão +1 exatamente quando o saldo muda, e versão inalterada quando não muda.
- **Critério impactado:** Integridade financeira.
- **Como verificar:** `TestBalanceChangeRequiresLedgerEntry`, `TestLedgerChainIsEnforced`, `TestWalletVersionRules`, `TestBalanceCannotBeNegative` e os mutantes "saldo confirmado sem lançamento", "cadeia do ledger não conferida", "banco aceita versão sem mudança de saldo" e "banco aceita saldo negativo".
- **Limitações:** o banco não confere que o estado da transação é `PROCESSED` quando o lançamento é gravado (a ordem das gravações é responsabilidade da UnitOfWork). O custo é uma consulta indexada por carteira em cada lançamento e no commit.

#### Máquina de estados também no banco

- **Contexto:** "Transação terminal não sofre novas transições. Replay consulta o resultado persistido sem reaplicar."
- **Opções consideradas:** (a) trigger de transição; (b) só no domínio.
- **Prós e contras:**
  - (a) impede que um UPDATE direto reabra ou altere uma transação concluída, o que corromperia o replay.
  - (b) é mais simples, mas depende só do código Go.
- **Escolha e justificativa:** (a). Trigger `BEFORE UPDATE/DELETE/TRUNCATE` em `wager_transactions`:
  - linha terminal não muda;
  - só valem as transições do §4.5;
  - identidade, valor, carteira, provedor, chave, hash, rodada, jogo e referência externa são imutáveis.
  
  CHECKs espelham as regras do domínio: operação interna vs. externa, zero só em `LOSS`, referência obrigatória em reversões e proibida em `BET`/`LOSS`, resultado coerente com o estado, e `failureCode` restrito ao catálogo (§4.9) conforme o estado.
- **Critério impactado:** Integridade financeira e Idempotência.
- **Como verificar:** `TestTransactionStateMachineInDatabase` e `TestDatabaseEnforcesDomainRules` (12 violações recusadas), e os mutantes "transação volta para PENDING" e "banco aceita código de falha desconhecido".
- **Limitações:** a lista de códigos e de moedas é duplicada entre o domínio e as constraints; um código novo exige uma migration.

#### Unicidade imposta pelo banco

| Índice ou constraint | Garantia |
|---|---|
| `wallets_player_currency_key` | uma carteira por `(playerId, currency)` |
| `wager_transactions_provider_key_uq` | idempotência por `(providerId, idempotencyKey)` |
| `wager_transactions_provider_external_uq` | a operação `(providerId, externalTransactionId)` não pode ser registrada com outra chave |
| `wager_transactions_opening_uq` | no máximo um `OPENING` (crédito inicial) por carteira |
| `wager_transactions_one_reversal_uq` | no máximo uma reversão `PROCESSED` por transação referenciada (D5) |
| `wallet_ledger_entries_wallet_transaction_key` | um lançamento por `(walletId, transactionId)` |
| `wallet_ledger_entries_wallet_version_key` | uma versão por carteira (cadeia sem bifurcação) |
| `inbox_messages_pkey` | uma mensagem por `(consumerName, messageId)` |

**Como verificar:** `TestIdempotencyConstraints`, `TestOneSuccessfulReversalPerReference` (um caso de uso com bug que ignora a reversão existente é barrado pelo índice) e `TestOneWalletPerPlayerAndCurrency`.

#### Representação de tipos e estados: `TEXT` + `CHECK`

- **Opções consideradas:** (a) `TEXT` com `CHECK (… IN (…))`; (b) `ENUM` do PostgreSQL.
- **Prós e contras:**
  - (a) evolui trocando a constraint numa migration reversível, e o valor lê igual ao domínio.
  - (b) é compacto, mas um valor de enum não pode ser removido, o que impede um `down` limpo.
- **Escolha:** (a).

#### Outbox e inbox

- **Payload da outbox:** coluna `json`, não `jsonb`. `json` preserva o texto exatamente como gerado por `event.Marshal`, então o evento republicado é byte a byte igual ao original. `jsonb` reordena chaves e normaliza o texto; `bytea` seria opaco para diagnóstico.
- **Imutabilidade do snapshot:** trigger impede alterar `event_id`, agregado, tipo, payload e ocorrência; depois de publicado, o evento não volta a pendente. Eventos não são apagados.
- **Colunas de retry:** `attempts`, `next_attempt_at` e `last_error`. O índice parcial de pendentes é `published_at IS NULL`.
- **Inbox:** `(consumer_name, message_id)` como chave, mais `payload_hash`, `received_at` e `completed_at`. A conclusão é registrada uma única vez (trigger), e `Register` com `ON CONFLICT DO NOTHING` devolve o registro existente em reentregas.
- **Como verificar:** `TestOpeningRoundTrip` (payload igual a `event.Marshal`), `TestOutboxSnapshotIsImmutable`, `TestInboxDeduplicates` e o mutante "payload da outbox pode mudar".
- **Pendente:** campos de disputa entre publishers e de recuperação de trabalho abandonado (Fase 8, D10).

#### Banco dos testes de integração: temporário por pacote

- **Contexto:** "Integração (containers reais: PostgreSQL […] — sem substituir toda a infraestrutura por mocks)."
- **Opções consideradas:** (a) banco temporário por pacote; (b) banco fixo `wallet_test`; (c) o mesmo banco da aplicação.
- **Prós e contras:**
  - (a) isola dos dados da aplicação e entre pacotes, e permite testar `down`/`up` num banco próprio.
  - (b) acumula dados entre execuções, porque o ledger não pode ser apagado, e o `down`/`up` competiria com os demais testes.
  - (c) polui o banco da aplicação e derrubaria a aplicação em execução no `down`.
- **Escolha e justificativa:** (a). O `TestMain` cria `wallet_test_<aleatório>` como `wallet_owner` (que tem `CREATEDB`), aplica as migrations com o `goose` como biblioteca, roda os testes com o papel real `wallet_app` e apaga o banco ao final.
- **Como verificar:**
```bash
docker compose run --rm tools go test -race -tags=integration ./internal/adapters/postgres/...
```
- **Limitações:** exige o PostgreSQL do Compose no ar. O `goose` como biblioteca traz dependências transitivas de outros drivers para o `go.mod`, usadas apenas pelos testes.

### 4.12 Casos de uso

`app.Service` reúne os casos de uso: `OpenWallet`, `SubmitOperation`, `ResolvePendingReference`, `DuePendingReferences`, `Reconcile`, `GetWallet`, `ListLedger`, `GetTransaction` e `GetTransactionByExternalID`. HTTP e SQS chamarão os mesmos métodos, com o mesmo `wagering.RequestInput`, então o hash, a idempotência e as regras são idênticos nos dois canais. O relógio é uma porta (`app.Clock`), o que deixa os testes de espera e de expiração determinísticos.

#### D14. Processamento síncrono para operações sem dependência

- **Contexto:** "Todo `PENDING` confirmado deve ter retomada durável por outra instância após interrupção. Operações sem dependências podem ser concluídas de forma síncrona, sem commit intermediário de aceite."
- **Opções consideradas:** (a) síncrono para tudo o que não depende de referência; (b) sempre assíncrono (commit de `PENDING` e processamento por worker).
- **Prós e contras:**
  - (a) usa um único commit por operação, que já nasce terminal.
  - (b) dobra commits e latência, e exige retomada de `PENDING` em todos os casos.
- **Escolha e justificativa:** (a).
  - `BET`, `WIN`, `LOSS`, `OPENING` e reversões com referência resolvida são processados e confirmados num único commit, já em `PROCESSED` ou `REJECTED`.
  - Só a reversão sem referência disponível é confirmada como `PENDING_REFERENCE`, com o agendamento durável, e retomada pelo worker por qualquer instância.
  - Como nenhuma transação é confirmada em `PENDING`, não há `PENDING` a retomar. Uma queda antes do commit não deixa nada gravado.
- **Critério impactado:** Integridade financeira e Mensageria e recuperação.
- **Como verificar:** `TestReplayReturnsOriginalResult` (resultado terminal na primeira resposta) e `TestReversalBeforeReferenceResolvedByWorker`.
- **Limitações:** o cenário "interromper após confirmar `PENDING` e antes de executar" não se aplica (nenhum `PENDING` é confirmado). O equivalente, a retomada de `PENDING_REFERENCE` por outra instância, é demonstrado por `TestRestartAllInstances` e `TestWorkersAcrossInstances` (§4.19).

#### Fluxo de idempotência e corrida entre requisições iguais

- **Contexto:** "Mesma chave + conteúdo equivalente → resultado persistido com `idempotentReplay: true`." / "Mesma aposta enviada 50 vezes em paralelo → um único débito."
- **Opções consideradas:** (a) busca sem lock, lock da carteira, nova busca e releitura em caso de colisão; (b) advisory lock por `(provider, chave)`; (c) reservar a chave com um INSERT de `PENDING`.
- **Prós e contras:**
  - (a) usa o lock que já existe por carteira, e o índice único é a rede de segurança.
  - (b) acrescenta um lock e trava chaves sem relação quando há colisão de hash.
  - (c) exige o commit intermediário de aceite, contradizendo a D14.
- **Escolha e justificativa:** (a). Em `SubmitOperation`:
  1. **Busca pela chave e, depois, pelo id externo**, sempre dentro do provedor e sem lock. Se encontrar, `CheckReplay` devolve o replay ou o conflito, sem travar a carteira.
  2. **`Lock` da carteira** (inexistente: `WALLET_NOT_FOUND`, nada persistido).
  3. **Nova busca sob o lock:** quem esperou na fila enxerga o que acabou de ser confirmado.
  4. **Carrega a referência**, processa no domínio e grava transação, lançamento, saldo e eventos num único commit.
  5. **Colisão residual** de índice único (por exemplo, a mesma chave usada para outra carteira) desfaz a transação. A nova tentativa refaz a busca e devolve o replay ou o conflito.
  
  O replay devolve o registro persistido, inclusive o saldo observado no processamento original, mesmo que a carteira tenha mudado depois.
- **Critério impactado:** Idempotência e Concorrência.
- **Como verificar:**
  - `TestFiftyIdenticalBetsInParallel`: 50 envios paralelos produzem 1 transação, 1 débito e 49 replays com o mesmo saldo.
  - `TestReplayReturnsOriginalResult`, `TestIdempotencyConflicts`, `TestRejectionIsPersistedAndReplayed`, `TestSameKeyFromAnotherProviderIsIndependent` e o mutante "replay não confere o payload".
- **Limitações:** até aqui, demonstrado com um processo; múltiplos processos e HTTP × SQS ficam para as Fases 7 e 10.

#### Erros corrigíveis não consomem a chave

- **Escolha:** erros de entrada (`KindValidation`) e carteira inexistente (`WALLET_NOT_FOUND`) não persistem nada. A mesma chave pode ser usada de novo depois de corrigida a requisição. A transação também não poderia ser gravada sem carteira, por causa da FK.
- **Como verificar:** `TestKeyNotConsumedByCorrectableErrors` e o mutante "carteira inexistente sem WALLET_NOT_FOUND".

#### Retry de falhas transitórias no caso de uso

- **Contexto:** "Falhas transitórias: retry com backoff." / "Indisponibilidade temporária do PostgreSQL" sem movimentação duplicada.
- **Opções consideradas:** (a) retry curto dentro do caso de uso; (b) sem retry interno, ficando para o cliente HTTP ou para o SQS.
- **Prós e contras:**
  - (a) absorve deadlocks e conflitos momentâneos sem afetar o provedor.
  - (b) é mais simples, mas um deadlock ocasional vira erro para o provedor.
- **Escolha e justificativa:** (a). `RetryPolicy` faz até 3 novas tentativas depois da primeira, esperando 50, 100 e 200 ms (±20%). Só repete erros em que nada foi confirmado: `ErrTransient`, `ErrConcurrentUpdate` e `ErrDuplicate` (que, relido, vira replay ou conflito). Rejeições de negócio e erros de entrada nunca são repetidos. Esgotadas as tentativas, o erro sobe, e o HTTP (Fase 6) ou o SQS (Fase 7) decide.
- **Como verificar:** `TestRetryPolicy`, `TestRetryStopsOnCancelledContext` e o mutante "retry não repete falha transitória".
- **Limitações:** a repetição é segura porque cada tentativa é uma transação nova e completa, e a idempotência no banco impede efeito duplicado.

#### D4. Espera por referência: 10 tentativas ou 10 minutos

- **Contexto:** "Worker tenta novamente com backoff exponencial, inclusive após reinicialização. Número máximo de tentativas ou TTL. Esgotado: finalizar como `REJECTED` com código de referência não encontrada e evento de rejeição."
- **Opções consideradas:** (a) 10 tentativas ou 10 minutos, o que vier primeiro; (b) só TTL de 30 minutos; (c) só 8 tentativas.
- **Prós e contras:**
  - (a) limita tanto o número de tentativas quanto o tempo.
  - (b) é mais tolerante, mas o provedor espera mais por uma resposta definitiva.
  - (c) tem duração real dependente do atraso do worker.
- **Escolha e justificativa:** (a), configurável por `REFERENCE_MAX_ATTEMPTS`, `REFERENCE_TTL`, `REFERENCE_BACKOFF_BASE` e `REFERENCE_BACKOFF_MAX`.
  - **Agendamento durável** em `wager_transactions` (`reference_attempts`, `next_reference_attempt_at`, `reference_expires_at`), por isso sobrevive a reinícios. Um CHECK exige agendamento em toda transação `PENDING_REFERENCE`.
  - **Backoff:** primeira tentativa do worker 1 s após a criação, depois 2, 4, 8, 16, 32 e 60 s (teto), com ±20% de jitter em aritmética inteira. A próxima tentativa nunca é agendada depois do prazo final.
  - **Contagem:** as tentativas contadas são as do worker. A tentativa sem sucesso que atinge o máximo, ou que acontece depois do prazo, finaliza a transação como `REJECTED` com `REFERENCE_NOT_FOUND` e emite `WagerTransactionRejected`.
  - **Sempre há uma última tentativa** antes de expirar: se o worker ficou parado além do prazo e a referência chegou nesse meio-tempo, a reversão é concluída em vez de rejeitada.
- **Critério impactado:** Mensageria e recuperação.
- **Como verificar:** `TestReferencePolicyBackoff`, `TestReferencePolicyNeverSchedulesPastExpiry`, `TestReferencePolicyExhaustion`, `TestPendingReferenceExpiresAfterMaxAttempts`, `TestPendingReferenceExpiresAfterTTL` e os mutantes "espera não respeita o máximo de tentativas", "espera ignora o TTL" e "backoff sem teto".
- **Limitações:** o loop do worker que chama `DuePendingReferences` e `ResolvePendingReference` periodicamente entra na Fase 8.

#### Resolução das pendências: só pelo worker

- **Opções consideradas:** (a) só pelo worker, no agendamento do backoff; (b) também na hora, no mesmo commit da referência; (c) na hora, antecipando o agendamento.
- **Prós e contras:**
  - (a) tem um caminho único e não acopla a operação da referência às reversões de terceiros.
  - (b) resolve mais rápido, mas cria dois caminhos e põe operações de terceiros na transação da `BET`.
  - (c) responde rápido mantendo um caminho único.
- **Escolha e justificativa:** (a). `ResolvePendingReference`:
  - lê a transação;
  - trava a carteira e depois a transação, na mesma ordem de locks do restante do sistema;
  - confere que ela continua em `PENDING_REFERENCE` e que a tentativa venceu;
  - processa no domínio e grava tudo num commit.
  
  Várias instâncias podem chamar para a mesma transação: só uma conclui, e as demais recebem `skipped`.
- **Como verificar:** `TestReversalBeforeReferenceResolvedByWorker` (inclusive `skipped` antes do horário), `TestConcurrentResolversProcessOnce` (8 chamadas simultâneas: 1 conclusão, 1 crédito) e o mutante "worker ignora o horário agendado".
- **Limitações:** a resolução leva até o próximo agendamento (no máximo 60 s depois de a referência chegar).

#### Rastreio persistido da requisição de origem

- **Contexto:** eventos com `correlationId` e `causationId`; logs com os identificadores disponíveis.
- **Opções consideradas:** (a) persistir `correlation_id` e `causation_id` da requisição que criou a transação; (b) gerar novos identificadores no worker.
- **Prós e contras:**
  - (a) faz os eventos emitidos depois pelo worker carregarem o mesmo `correlationId` da requisição HTTP ou SQS original, o que permite seguir a operação de ponta a ponta.
  - (b) não muda o schema, mas perde essa ligação.
- **Escolha e justificativa:** (a). A migration 00005 acrescenta as colunas, imutáveis por trigger. As linhas anteriores recebem `'unknown'`.
- **Como verificar:** `TestReversalBeforeReferenceResolvedByWorker` (o evento do worker tem o `correlationId` da requisição original).

#### Reconciliação: REPEATABLE READ somente leitura

- **Contexto:** "Reconstrói o saldo a partir do ledger, incluindo a abertura, em uma visão consistente dos dados. `difference = storedBalance − calculatedBalance`. Divergências reportadas na resposta, nos logs e em métrica. Não altera o saldo."
- **Opções consideradas:** (a) transação `REPEATABLE READ READ ONLY`; (b) lock `FOR SHARE` na carteira; (c) uma única query.
- **Prós e contras:**
  - (a) dá um snapshot único sem bloquear escritores e não aceita escrita.
  - (b) faz as apostas daquela carteira esperarem.
  - (c) fica frágil se a reconciliação crescer além de uma consulta.
- **Escolha e justificativa:** (a), via `UnitOfWork.View`. A soma de créditos − débitos é feita no banco em `NUMERIC` e convertida para `int64` com checagem de faixa, sem ponto flutuante. Uma divergência resulta em `consistent = false` na resposta e em log `error` com carteira, valores e número de lançamentos; a métrica entra na Fase 9.
- **Critério impactado:** Integridade financeira e Observabilidade.
- **Como verificar:** `TestReconciliation`. O teste simula uma corrupção: o dono do schema desliga os triggers e altera o saldo. A divergência é detectada e logada, e o saldo não é alterado. O mutante "reconciliação sempre consistente" também é morto.
- **Limitações:** não corrige divergências; a correção exige novos lançamentos.

#### Paginação do ledger

- **Cursor:** `base64url({"v":1,"after":<walletVersion>})`. É opaco para o cliente, com ordenação estável por `walletVersion`. Cursor malformado, de outra versão de formato ou com versão negativa resulta em `INVALID_REQUEST`. Ele não é assinado: alterá-lo só muda a página dentro da carteira já autorizada. A alternativa assinada (HMAC) exigiria um segredo sem ganho real, e a versão numérica direta não seria opaca.
- **Tamanho:** padrão 50 (o exemplo do enunciado). Valores fora de 1 a 200 resultam em `INVALID_REQUEST`, sem ajuste silencioso. A busca pede um lançamento a mais, para saber se existe página seguinte sem devolver uma página vazia.
- **Como verificar:** `TestLedgerPagination`, `TestCursor` e os mutantes "cursor repete o último lançamento" e "cursor aceita versão negativa".

#### Consultas

- `GetWallet`, `GetTransaction`, `GetTransactionByExternalID` e `ListLedger` usam `View`.
- `GetTransactionByExternalID` é sempre escopada pelo provedor; o provedor errado recebe `TRANSACTION_NOT_FOUND`, sem revelar a existência da operação.
- A autorização por provedor em `GetTransaction` (busca por id interno) é feita pela camada HTTP (Fase 6).
- **Como verificar:** `TestTransactionLookupsAreProviderScoped`.

### 4.13 HTTP e autenticação

A API usa `net/http` (`ServeMux` com padrões de método e rota do Go 1.22+), sem router externo. Health checks são públicos; as demais rotas exigem um access token `client_credentials` do Keycloak com o scope da operação.

| Rota | Scope | Sucesso |
|---|---|---|
| `POST /wallets` | `wallets:write` | 201 + `Location` |
| `GET /wallets/{walletId}` | `wallets:read` | 200 |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | `wallets:read` | 200 |
| `POST /wallets/{walletId}/reconciliation` | `wallets:reconcile` | 200 |
| `POST /wagering/transactions` | `wagering:write` | 200 / 422 / 202 (abaixo) |
| `GET /wagering/transactions/{transactionId}` | `wagering:read` | 200 |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | `wagering:read` | 200 |
| `GET /health/live`, `GET /health/ready` | — | 200 / 503 |

#### D8. Validação de token: `coreos/go-oidc` v3

- **Contexto:** "IdP externo OAuth 2.0/OIDC […] `client_credentials` para comunicação entre serviços. […] Escolha do IdP, validação de credenciais e modelo de permissões justificados."
- **Opções consideradas:** (a) `coreos/go-oidc` v3; (b) `lestrrat-go/jwx` v3; (c) `golang-jwt` + `keyfunc`.
- **Prós e contras:**
  - (a) é a biblioteca OIDC de referência em Go, com cache e rotação de chaves e verificação de assinatura, emissor, audiência e expiração.
  - (b) é uma suíte JOSE completa, com mais superfície do que o necessário.
  - (c) exige conferir emissor e audiência à mão.
- **Escolha e justificativa:** (a). O verificador exige:
  - assinatura **RS256** (algoritmo fixo) com as chaves do JWKS;
  - `iss` igual a `OIDC_ISSUER`;
  - `aud` contendo `wallet-api`;
  - `exp` válido.
  
  O JWKS é buscado sob demanda em `OIDC_JWKS_URL`, então o processo sobe com o Keycloak fora do ar; as requisições autenticadas dão 401 até ele voltar. O motivo exato da rejeição vai só para o log.
- **IdP:** Keycloak 26.8.1 no Compose, com o realm `wallet` importado de [deploy/keycloak/wallet-realm.json](deploy/keycloak/wallet-realm.json).
- **Critério impactado:** Modelagem e arquitetura e Testes.
- **Como verificar:** `TestAuthenticationFailures`, com o Keycloak real: token ausente, esquema não Bearer, token malformado, assinatura adulterada, audiência errada e token expirado dão 401, sem efeito financeiro. Mutantes "audiência não conferida", "expiração não conferida" e "assinatura não conferida".
- **Limitações:** não há revogação ativa (introspecção); um token vale até expirar (5 min por padrão no realm).

#### D8. Emissor fixo no Keycloak

- **Contexto:** o `iss` de um token do Keycloak depende do endereço usado para pedi-lo (`localhost:8180` pelo host, `keycloak:8080` pela rede do Compose).
- **Opções consideradas:** (a) hostname fixo; (b) tokens só pela rede interna.
- **Prós e contras:**
  - (a) aceita tokens pedidos de qualquer lado.
  - (b) faria um token pedido pelo host (curl, Postman) ser rejeitado.
- **Escolha e justificativa:** (a). `KC_HOSTNAME=http://localhost:8180` com `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`. Todo token tem `iss = http://localhost:8180/realms/wallet`, e os endpoints de backchannel (token, JWKS) respondem pelo endereço usado na requisição. A aplicação valida esse emissor e busca o JWKS em `http://keycloak:8080/...`.
- **Como verificar:** os testes de integração pedem tokens por `keycloak:8080`, e o fluxo manual do README os pede por `localhost:8180`; os dois são aceitos.
- **Limitações:** mudar a porta publicada exige ajustar `KEYCLOAK_HOST_PORT` (o Compose deriva o emissor dela).

#### D8. Identificação do provedor: claim `provider_id`

- **Contexto:** "Isolamento entre provedores, inclusive em consultas e replays."
- **Opções consideradas:** (a) claim fixo `provider_id` por client, via *hardcoded claim mapper*; (b) `client_id` (`azp`) como providerId.
- **Prós e contras:**
  - (a) desacopla o nome do client do id de negócio e permite vários clients por provedor.
  - (b) dispensa o mapper, mas acopla os dois.
- **Escolha e justificativa:** (a).
  - **Envio:** o `providerId` do corpo precisa ser igual ao do token; diferente resulta em 403, e nada é processado (nem replay).
  - **Consulta por id:** só devolve transações do próprio provedor. As de outro provedor, e o `OPENING` interno, dão 404 sem revelar a existência.
  - **Consulta por id externo:** a rota com `providerId` diferente do token dá 403.
  - Um token com scope de wagering mas sem `provider_id` dá 403.
- **Como verificar:** `TestProviderIsolation` e os mutantes "provedor opera em nome de outro", "provedor lê transação de outro" e "rota por id externo sem escopo de provedor".

#### D8. Modelo de permissões: scopes por operação

- **Opções consideradas:** (a) scopes OAuth por operação; (b) dois papéis grosseiros (provider e internal); (c) realm roles por operação.
- **Prós e contras:**
  - (a) dá menor privilégio por operação e é o mecanismo padrão do `client_credentials`.
  - (b) dá a qualquer client interno acesso a tudo.
  - (c) é equivalente a (a), mas fora do padrão OAuth para esse fluxo.
- **Escolha e justificativa:** (a). Scopes: `wagering:write`, `wagering:read`, `wallets:read`, `wallets:write` e `wallets:reconcile`. Token sem o scope da rota dá 403 com `WWW-Authenticate: Bearer error="insufficient_scope"`, e o handler não roda.
- **Clients provisionados** (segredos `<client>-secret`, somente locais):

| Client | `provider_id` | Scopes | Uso |
|---|---|---|---|
| `wallet-backoffice` | — | `wallets:*` | operações internas |
| `provider-a` | `provider-a` | `wagering:*` | provedor |
| `provider-b` | `provider-b` | `wagering:*` | provedor (testes de isolamento) |
| `provider-short-lived` | `provider-a` | `wagering:*` | teste de expiração (token de 2s) |
| `no-audience` | `provider-a` | `wagering:*` | teste de audiência (sem `wallet-api`) |

- **Como verificar:** `TestAuthorizationByScope` (6 combinações proibidas; nenhuma movimenta saldo) e o mutante "scope não conferido".
- **Limitações:** o realm é gerado para uso local, com segredos fixos; em produção os segredos viriam de um cofre.

#### Leitura de carteira e ledger: só clients internos

- **Opções consideradas:** (a) só clients com `wallets:read`; (b) provedor lê carteiras em que já operou; (c) qualquer autenticado.
- **Prós e contras:**
  - (a) não exige inventar uma regra de "dono" da carteira, que o enunciado não define.
  - (b) expõe ao provedor operações de outros provedores no ledger.
  - (c) viola o isolamento.
- **Escolha e justificativa:** (a). O provedor vê o saldo resultante nas respostas das próprias operações.

#### D6. Mapeamento para HTTP

| Situação | Status | Corpo |
|---|---|---|
| Entrada inválida (`KindValidation`) | 400 | problem+json |
| Token ausente ou inválido | 401 + `WWW-Authenticate` | problem+json `UNAUTHENTICATED` |
| Sem scope, provedor divergente | 403 | problem+json `FORBIDDEN` |
| Recurso inexistente (`KindNotFound`) | 404 | problem+json |
| Conflito (`KindConflict`) | 409 | problem+json |
| `Content-Type` diferente de JSON | 415 | problem+json `UNSUPPORTED_MEDIA_TYPE` |
| Operação **rejeitada** e persistida (`REJECTED`) | **422** | corpo da transação, com `failureCode` |
| Operação **pendente** (`PENDING_REFERENCE`) | **202** + `Location` | corpo da transação |
| Operação processada | 200 | corpo da transação |
| Falha transitória com retries esgotados | 503 + `Retry-After: 1` | problem+json `UNAVAILABLE` |
| Invariante violada ou erro inesperado | 500 | problem+json, sem detalhes internos |

- **Formato de erro:** RFC 9457 (`application/problem+json`) com `type`, `title`, `status` e `detail`, mais as extensões `code` (estável) e `correlationId`. A alternativa (JSON próprio) foi descartada por não ser padrão.
- **Rejeição:** 422 com o mesmo corpo do sucesso (`transactionId`, `status: REJECTED`, `failureCode`, saldo observado), distinguível de 400 e 409. Responder 200 com `status: REJECTED` foi descartado, porque o enunciado pede códigos distinguíveis.
- **Replay:** mesmo status e corpo do original, com `idempotentReplay: true`. Uma rejeição reproduzida continua 422. "Sempre 200" foi descartado, porque esconderia a rejeição de quem olha o status.
- **Como verificar:** `TestOperationResponses`, `TestInputAndConflictErrors`, `TestErrorMapping`, `TestStatusFor` e o mutante "rejeição respondida como sucesso".

#### Contrato de entrada

- **`Idempotency-Key` obrigatório** em `POST /wagering/transactions`. Ausente resulta em 400, com uma mensagem que aponta o header. O servidor nunca substitui a chave por outra calculada.
- **JSON estrito:**
  - `Content-Type: application/json`;
  - até 64 KiB;
  - **campos desconhecidos rejeitados** (um nome de campo errado não vira, em silêncio, uma operação diferente);
  - um único objeto por corpo.
  
  A alternativa de ignorar campos extras foi descartada.
- **Money:** `amount` precisa ser string. Um número JSON (que passaria por ponto flutuante) resulta em `INVALID_AMOUNT`.
- **Como verificar:** `TestInputAndConflictErrors`, `TestDecodeJSON`, `TestMoneyInputRejectsNumbers` e os mutantes "campos desconhecidos aceitos" e "Idempotency-Key opcional".

#### Correlação

- **Opções consideradas:** (a) usar o `X-Correlation-Id` do cliente quando válido e gerar um UUIDv7 caso contrário; (b) sempre gerar.
- **Escolha e justificativa:** (a). Assim o provedor pode correlacionar com os sistemas dele. O id volta no header da resposta e é gravado na transação, nos eventos da outbox e nos corpos de erro. Um valor inválido (vazio, com mais de 255 bytes ou com caracteres de controle) é substituído.
- **Como verificar:** `TestCorrelationID` e o mutante "X-Correlation-Id do cliente ignorado".

#### Outros comportamentos

- Panic em handler vira 500 problem+json, sem derrubar o processo (`TestRecovererReturnsProblem`).
- A readiness continua cobrindo só PostgreSQL e SQS, como pede o enunciado; o Keycloak não entra nela.

### 4.14 Consumidor SQS

O consumidor lê `wager-transactions.fifo` e chama o mesmo caso de uso do HTTP (`HandleMessage` envolve `SubmitOperation`), com o mesmo `RequestInput`, o mesmo hash e as mesmas regras de idempotência.

#### Inbox na mesma transação SQL

- **Contexto:** "Na entrada SQS, o registro da inbox e a conclusão durável do tratamento compartilham a mesma transação SQL das alterações de domínio, do ledger e dos eventos." / "Remover a mensagem somente após o commit do tratamento durável."
- **Implementação:** `Service.HandleMessage` faz, num único commit:
  1. registra `(consumerName, messageId, hash)` na inbox;
  2. processa a operação (transação, ledger, saldo e eventos);
  3. marca a mensagem como concluída na inbox.
  
  Só depois do commit o consumidor remove a mensagem da fila. Se a mensagem já está na inbox, ela é removida sem reprocessar. A `idempotencyKey` continua valendo à parte: mensagens diferentes com a mesma chave viram replay, inclusive entre HTTP e SQS.
- **Hash da inbox:** SHA-256 do corpo recebido (`v1:<hex>`). Uma reentrega do SQS traz o corpo idêntico; o mesmo `messageId` com corpo diferente resulta em `MESSAGE_HASH_MISMATCH` e vai para a DLQ. A alternativa de ignorar e remover foi descartada, porque o conteúdo divergente sumiria sem rastro.
- **Correlação:** o `correlationId` e o `causationId` dos eventos são o `messageId` do envelope.
- **Como verificar:**
  - `TestConsumesOperation` (inbox concluída e eventos correlacionados);
  - `TestRedeliveredMessageIsNotReprocessed` (o mesmo envelope duas vezes, sem a deduplicação do SQS, gera um débito);
  - `TestDifferentMessagesSameKey`, `TestHTTPAndSQSShareIdempotency` (HTTP→SQS e SQS→HTTP) e `TestMessageHashMismatchGoesToDeadLetterQueue`;
  - mutantes "inbox não conclui a mensagem" e "inbox ignora hash divergente".

#### D9. Parâmetros da fila

| Parâmetro | Valor | Motivo |
|---|---|---|
| `MessageGroupId` | `walletId` | Ordem por carteira e paralelismo entre carteiras; casa com o lock por carteira (D1). Uma mensagem com problema só atrasa a própria carteira. `providerId` concentraria todas as carteiras de um provedor numa fila só; `playerId` misturaria moedas. |
| `MessageDeduplicationId` | `messageId` do envelope | Primeira camada (janela de 5 min do SQS) contra reenvios do produtor. As garantias reais são a inbox e a idempotência no banco. Usar `idempotencyKey` descartaria em silêncio, no broker, um reenvio com conteúdo diferente, que deveria dar conflito. |
| `ContentBasedDeduplication` | `false` | A identidade é o `messageId`, não o conteúdo. |
| Visibility timeout | 30s, renovado a cada 10s durante o processamento (heartbeat) | Uma mensagem longa não reaparece para outra instância no meio do tratamento. |
| `maxReceiveCount` | 5, com redrive para `wager-transactions-dlq.fifo` | Tolera indisponibilidades curtas sem prender mensagens válidas para sempre. |
| Backoff de falha transitória | visibilidade de 2·2ⁿ⁻¹ s (2, 4, 8, 16), teto de 60s | A mensagem não é removida e volta mais tarde. |
| Long polling | 20s, até 10 mensagens por recebimento | |
| Retenção da DLQ | 14 dias | |

Todos os valores são configuráveis (`SQS_*`). As opções descartadas foram: 60s com 3 recebimentos e sem backoff (lento, e uma queda curta do banco mandaria mensagens válidas para a DLQ) e 10 recebimentos (prende por mais tempo as mensagens com problema persistente).

#### Desfecho de cada mensagem

| Resultado do tratamento | Ação |
|---|---|
| `PROCESSED`, `REJECTED` (regra de negócio) ou `PENDING_REFERENCE` | Commit e remoção. A rejeição é terminal; a pendência segue com o worker de referências. |
| Mensagem já na inbox com o mesmo hash | Remoção, sem reprocessar. |
| Envelope inválido (`INVALID_MESSAGE`), entrada inválida, carteira inexistente, conflito, hash divergente ou invariante violada | Envio à DLQ com os atributos `ErrorCode`, `ErrorDetail`, `SourceQueue` e `OriginalMessageId`, e depois remoção. |
| Falha transitória (banco indisponível, deadlock, timeout) | Não remove; a visibilidade passa a valer o backoff. Na 5ª recepção sem sucesso, o redrive do SQS move a mensagem para a DLQ (sem `ErrorCode`). |
| Falha ao enviar para a DLQ | Não remove; tratada como transitória. |

- **Mensagens inválidas vão à DLQ na hora**, em vez de esperar o redrive. Assim não bloqueiam o grupo FIFO da carteira durante 5 recebimentos, e o motivo fica gravado junto da mensagem.
- **Envelope estrito:** um único objeto JSON, sem campos desconhecidos em nenhum nível, `type = WagerTransactionRequested`, `messageId` válido, `occurredAt` em RFC 3339 e `amount` como string.
- **Como verificar:**
  - `TestInvalidMessagesGoToDeadLetterQueue` (8 casos, cada um com o seu `ErrorCode`, sem efeito financeiro);
  - `TestBusinessOutcomesAreAcknowledged`, `TestTransientFailureIsRetried` e `TestExhaustedRetriesGoToDeadLetterQueue` (exatamente 5 chamadas e depois a DLQ pelo redrive);
  - testes unitários `TestParseRejectsInvalidEnvelopes`, `TestPermanentVersusTransient` e `TestConsumerBackoff`.

#### Queda entre o commit e a remoção

Se o processo cair depois do commit e antes de remover a mensagem, ela reaparece quando o visibility timeout vence. A inbox reconhece o `messageId` e a mensagem é removida sem reprocessar. `TestCrashAfterCommitBeforeDelete` simula exatamente essa queda e confere que existe um único débito. Uma falha ao remover tem o mesmo efeito.

#### Concorrência dentro de uma instância

- **Opções consideradas:** (a) paralelo entre grupos e em ordem dentro de cada grupo; (b) sequencial por lote; (c) paralelo total.
- **Prós e contras:**
  - (a) processa carteiras diferentes ao mesmo tempo sem quebrar a ordem de uma carteira.
  - (b) faz carteiras independentes esperarem umas pelas outras.
  - (c) poderia processar fora de ordem duas mensagens da mesma carteira no mesmo lote.
- **Escolha:** (a). Pollers configuráveis (`SQS_CONSUMER_POLLERS`, padrão 2). Cada lote é separado por `MessageGroupId`; grupos rodam em paralelo e as mensagens de um mesmo grupo são tratadas na ordem de chegada.
- **Falha no meio de um grupo:** as mensagens seguintes do grupo voltam junto com a que falhou. A ordem já é garantida pelo SQS FIFO, que não entrega mensagens de um grupo enquanto outra dele está em voo; reagendá-las só evita que esperem o visibility timeout inteiro.
- **Como verificar:** `TestOrderWithinWallet` (aposta de 80 e depois de 30 numa carteira de 100: a primeira passa e a segunda é rejeitada) e `TestHeartbeatKeepsMessageInvisible` (processamento de 5s com visibilidade de 2s: o handler é chamado uma única vez).

#### Encerramento (SIGTERM)

- **Opções consideradas:** (a) concluir o que está em andamento e liberar o restante; (b) liberar tudo imediatamente.
- **Escolha:** (a). O consumidor para de buscar mensagens na hora. As que estão em processamento terminam dentro de `SHUTDOWN_TIMEOUT`. As recebidas e ainda não iniciadas têm a visibilidade zerada e voltam para outra instância sem esperar o timeout. Se o prazo estourar, o processamento é cancelado: a transação SQL é desfeita e a mensagem volta pelo timeout.
- **Ordem no Fx:** o consumidor para antes do pool do PostgreSQL (ordem inversa de inicialização), então o trabalho em andamento termina com o banco disponível.
- **Como verificar:** `TestGracefulShutdown` (3 mensagens da mesma carteira: exatamente 1 concluída e 2 visíveis de novo na hora) e `TestStopDeadlineCancelsInFlight` (prazo estourado: nada gravado, nem a inbox, e a mensagem volta à fila).

#### Acesso ao broker

- **Contexto:** "Acesso à mensageria controlado por credenciais e política do broker, preservando as validações de domínio no consumidor."
- **Implementação:** o init do LocalStack cria os principals `wager-producer` e `wallet-consumer` e registra as políticas nas filas:
  - na fila principal, só o produtor envia; só o consumidor recebe, remove e altera a visibilidade;
  - na DLQ, o consumidor envia mensagens inválidas, e o redrive é aceito apenas a partir da fila principal.
- **Limitação verificada:** o LocalStack 4.14 comunitário **não aplica IAM**. Num teste com `ENFORCE_IAM=1`, um usuário sem nenhuma permissão conseguiu enviar mensagem. As políticas ficam registradas e valem tal qual na AWS, mas aqui não há teste de acesso negado.
- **Independente disso**, o consumidor valida cada mensagem como se viesse de fonte não confiável: envelope estrito, todas as regras de domínio, e carteira e jogador conferidos.
- **Provedor:** pela fila, o `providerId` da mensagem é aceito com base na política do broker, já que não há token. Na AWS, cada provedor teria o seu principal de produtor.

#### Outros comportamentos

- Na partida, o consumidor resolve as URLs das filas. Se o SQS estiver fora do ar nesse momento, a inicialização falha, e no Compose a aplicação só sobe depois do LocalStack saudável. Uma indisponibilidade **durante** a execução apenas faz o recebimento repetir a cada segundo.
- `SQS_CONSUMER_ENABLED=false` desliga o consumidor, por exemplo para uma instância que só atende HTTP.
- **A DLQ também é FIFO.** Quem a consome precisa remover cada mensagem lida, porque, enquanto houver mensagens de um grupo em voo, as seguintes do mesmo grupo não são entregues. Isso foi descoberto pelo próprio teste da DLQ.

### 4.15 Outbox e workers

Todas as escolhas desta seção foram feitas explicitamente, uma a uma. Os eventos gravados na outbox (no mesmo commit da operação, §4.11) são publicados por um `app.OutboxRelay`, que depende de duas portas: `OutboxRepository` (PostgreSQL) e `EventPublisher` (SNS). Os loops com ciclo de vida ficam em `internal/worker`.

#### D10. Destino: tópico SNS FIFO com fila assinante

- **Contexto:** "Eventos externos publicados somente após o commit da transação que os originou." / "Provisionar o destino dos eventos e documentar contratos de roteamento e consumo."
- **Opções consideradas:** (a) tópico SNS FIFO com fila assinante; (b) fila SQS FIFO direta; (c) EventBridge.
- **Prós e contras:**
  - (a) permite vários consumidores independentes (fan-out), cada um com a sua fila.
  - (b) é mais simples, mas tem um único consumidor lógico.
  - (c) tem suporte parcial no LocalStack comunitário e nenhuma garantia FIFO.
- **Escolha e justificativa:** (a). Recursos provisionados:
  - tópico `wallet-events.fifo`, sem deduplicação por conteúdo;
  - fila assinante `wallet-events-audit.fifo`, com **entrega raw**: recebe exatamente o envelope gravado na outbox, byte a byte. A alternativa, o envelope do SNS, obrigaria cada consumidor a desembrulhar a mensagem;
  - DLQ `wallet-events-audit-dlq.fifo`, ligada pela **redrive policy da assinatura**: se o SNS não conseguir entregar à fila, o evento não se perde;
  - política na fila: só o tópico envia.
  
  Antes de implementar, verifiquei no LocalStack 4.14 três comportamentos: a entrega FIFO para a fila FIFO, a deduplicação por id e o redrive da assinatura.
- **Contrato de roteamento:**

| Campo | Valor |
|---|---|
| `MessageGroupId` | `aggregateId` (transação ou carteira): ordem por agregado no broker |
| `MessageDeduplicationId` | `eventId`: republicações dentro de 5 minutos são descartadas pelo SNS |
| Atributo `eventType` (String) | tipo do evento, para filtro por assinatura |
| Atributo `eventVersion` (Number) | versão do contrato do evento |
| Corpo | o envelope do §4.10 (`eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId`, `occurredAt`, `version`, `data`) |

  A alternativa de usar o `walletId` como grupo para todos os eventos foi descartada: daria uma ordem mais forte, porém com menos paralelismo, e exigiria extrair o `walletId` de cada payload.
- **Contrato de consumo:** a entrega é **at-least-once**. Depois da janela de 5 minutos do SNS, o consumidor deve deduplicar pelo `eventId`.
- **Critério impactado:** Mensageria e recuperação.
- **Como verificar:** `TestEventsArePublishedAsCommitted` (corpo idêntico ao da outbox, grupo, tipo e versão), `TestSubscriptionDeadLetters`, e os mutantes "grupo do broker diferente do agregado" e "republicação com outro id de deduplicação".
- **Limitações:** a deduplicação do broker vale só por 5 minutos.

#### Disputa entre publishers: reserva com lease

- **Opções consideradas:** (a) reserva com lease; (b) lock durante a publicação; (c) advisory lock por partição.
- **Prós e contras:**
  - (a) não segura transação nem conexão durante a chamada de rede e permite recuperar trabalho abandonado.
  - (b) mantém a transação aberta durante a chamada ao broker.
  - (c) exige coordenar partições.
- **Escolha e justificativa:** (a). A migration 00006 acrescenta `claimed_by`, `claimed_until` e `sequence`.
  - **Reserva:** um `UPDATE … WHERE event_id IN (SELECT … FOR UPDATE SKIP LOCKED)` reserva até 50 eventos para o publisher, por 30 s, e confirma na hora. A publicação acontece fora da transação.
  - **Identidade do publisher:** `hostname` mais um sufixo aleatório, legível em diagnóstico e único mesmo com várias cópias no mesmo host. A alternativa, um UUID puro, não indicaria a instância.
  - **Renovação:** enquanto publica, o publisher renova o lease a cada 10 s, então outro só assume se o dono parar de renovar (ou seja, se morreu). A alternativa sem renovação geraria mais republicações sob lentidão.
  - **Abandono:** com o lease vencido, outro publisher assume.
  - **Marcação:** `published_at`, `attempts + 1` e a liberação da reserva, condicionados a `published_at IS NULL`.
- **Como verificar:**
  - `TestTwoPublishersCompete` (2 publishers, 40 eventos, exatamente 40 chamadas ao broker);
  - `TestAbandonedClaimIsRecovered` (queda depois da reserva: ninguém assume antes do lease; depois, outro publica);
  - `TestLeaseIsRenewedDuringSlowPublish` (publicação de 2,5 s com lease de 1 s: ninguém mais assume);
  - mutantes "reserva ignora lease de outro publisher" e "lease não é renovado".

#### Ordem por agregado

- **Opções consideradas:** (a) garantir a ordem por agregado; (b) sem garantia, com o consumidor ordenando.
- **Escolha e justificativa:** (a). A reserva só pega o evento pendente **mais antigo** de cada agregado, e apenas se não houver evento anterior do mesmo agregado ainda não publicado (em andamento, em backoff ou reservado por outro). Uma falha segura os eventos seguintes do mesmo agregado até ela sair.
- **Critério de ordem:** a coluna `sequence` (identity), e não `occurredAt`. Os eventos de uma carteira são inseridos sob o lock da carteira, então a sequência segue a ordem real dos commits, sem depender do relógio de várias instâncias.
- **Tamanho da reserva:** um evento por agregado por lote. A alternativa, reservar a cadeia contígua do agregado, daria mais vazão em rajadas, mas com consulta e tratamento de falha parcial mais complexos. Quando há trabalho, o publisher volta na hora, sem esperar a ociosidade.
- **Concorrência no lote:** agregados diferentes em paralelo, até 8 publicações simultâneas.
- **Como verificar:** `TestOrderPerAggregate` (o evento da versão 3 falha duas vezes e, mesmo assim, os `WalletBalanceChanged` chegam na ordem 1 a 6) e o mutante "reserva ignora a ordem por agregado".

#### Falha no broker: backoff sem desistir

- **Opções consideradas:** (a) backoff sem limite de tentativas; (b) limite de tentativas com "estacionamento" manual.
- **Escolha e justificativa:** (a). Perder um evento confirmado é proibido pelo enunciado, e com a ordem por agregado um evento estacionado travaria os seguintes. A espera é de 1 s·2ⁿ com teto de 5 min e jitter de ±20%. `attempts`, `last_error` (até 500 caracteres) e `next_attempt_at` ficam registrados, e a reserva é liberada.
- **Como verificar:** `TestBrokerUnavailableIsRetriedWithBackoff` (falha registrada, nada é reservado de novo antes do backoff, e depois tudo é entregue) e os mutantes "reserva ignora o backoff", "falha de publicação sem agendar nova tentativa" e "falha de publicação marcada como publicada".

#### Queda entre commit e publicação, e entre publicação e marcação

- **Antes da reserva:** o evento fica pendente e qualquer publisher o pega.
- **Depois da reserva e antes de publicar:** o lease vence e outro publisher assume (`TestAbandonedClaimIsRecovered`, com o gancho `SetAfterClaim`).
- **Depois de publicar e antes de marcar:** outro publisher republica com o **mesmo `eventId` e os mesmos bytes**. O registro das chamadas mostra as duas publicações idênticas, e a fila assinante recebe uma única cópia, porque o broker deduplica (`TestRepublishAfterCrashKeepsEventID`, com o gancho `SetAfterPublish`).
- **Nada é publicado antes do commit:** os eventos de uma transação aberta são invisíveis ao publisher (`TestNothingIsPublishedBeforeCommit`).
- **Ganchos de teste:** `SetAfterClaim` e `SetAfterPublish` foram escolhidos no lugar de um publicador falso porque só um gancho alcança o intervalo entre publicar e marcar. As quedas reais (SIGKILL de um publisher com eventos reservados) estão em `TestKillPublisherHoldingClaims` (§4.19).

#### Worker de referências pendentes

- **Parâmetros:** a cada 1 s, busca até 50 pendências vencidas.
- **Paralelismo:** carteiras diferentes em paralelo (até 8 por instância) e as da mesma carteira em sequência. Várias instâncias podem rodar ao mesmo tempo; o lock da carteira e a checagem do agendamento garantem uma única conclusão (§4.12).
- **Erro numa pendência:** é logado em `warn` e **não conta como tentativa da D4**. Assim, uma queda longa do banco não faz reversões expirarem antes da hora.
- **Como verificar:** `TestReferenceWorkerResolvesPendingReversals` (dois loops simultâneos, duas carteiras: cada reversão com um único lançamento) e `TestReferenceWorkerExpiresMissingReferences`.

#### Execução, configuração e encerramento

- **Processo:** os workers rodam **no mesmo processo** da API e do consumidor, cada um com uma flag (`OUTBOX_PUBLISHER_ENABLED`, `REFERENCE_WORKER_ENABLED`). Uma imagem só, com papéis separáveis por configuração. As alternativas (subcomando separado ou serviço separado no Compose) duplicariam a composição ou o deploy.
- **Acordar o publisher:** só polling. Com o lote vazio, espera 500 ms; havendo trabalho, consulta de novo na hora. LISTEN/NOTIFY foi descartado por exigir uma conexão dedicada e outro caminho de falha.
- **Pool do PostgreSQL:** `DB_MAX_CONNS`, padrão 20, o suficiente para 8 resoluções, 8 publicações, o consumidor e o HTTP. Com 3 instâncias, são 60 conexões, abaixo do limite padrão de 100.
- **Readiness:** continua só com PostgreSQL e SQS. Com o SNS fora do ar, a API e o consumidor seguem aceitando operações, e os eventos aguardam na outbox.
- **Retenção:** eventos publicados são mantidos; o trigger e os privilégios já impedem `DELETE`.
- **Encerramento:** os loops param de iniciar trabalho novo. O que está em andamento termina dentro de `SHUTDOWN_TIMEOUT`, e o publisher **libera na hora** as reservas que não começou, para outra instância publicar sem esperar o lease (`TestStopReleasesUnstartedClaims`, `TestOutboxLoopLifecycle` e o mutante "encerramento não libera as reservas").
- **Logs:** sucessos em `debug` (com `eventId`, `eventType` e `aggregateId`) e falhas em `warn`.

### 4.16 Composição com Fx e ciclo de vida

- **Contexto:** "Fx compõe configuração, conexões, repositórios, casos de uso, handlers e workers […] fechamento das dependências depois dos componentes que as utilizam."
- **Implementado até aqui:**
  - A configuração é carregada e validada **antes** do Fx. Uma configuração inválida encerra o processo com código 1, antes de abrir qualquer conexão.
  - Módulos `logging`, `metrics`, `postgres`, `sqs`, `app`, `httpapi` e `worker` (`fx.Module` / `fx.Provide` / `fx.Invoke`), com logs do Fx em JSON via `fxevent.SlogLogger`.
  - As checagens de readiness são reunidas por um *value group* (`group:"readiness"`), e cada adaptador contribui com a sua.
  - O servidor HTTP abre o listener no `OnStart`, de modo que uma porta ocupada faz a inicialização falhar.
  - No `OnStop`, a readiness passa a falhar e o servidor drena as requisições dentro de `SHUTDOWN_TIMEOUT` (`fx.StopTimeout`).
  - O pool do PostgreSQL é fechado depois do servidor (ordem inversa de inicialização do Fx).
- **Raiz de composição:** `internal/bootstrap.Options(cfg)` reúne `fx.Supply(cfg)`, `fx.StopTimeout`, o logger do Fx e os módulos. O `main` só carrega a configuração e chama `fx.New(bootstrap.Options(cfg)).Run()`; o teste usa exatamente a mesma composição. O pacote fica fora de `internal/app` porque os adaptadores importam `app`, e o inverso criaria um ciclo.
- **Ordem real dos hooks:** o Fx registra os hooks na ordem em que **constrói** os componentes, e essa ordem vem das dependências, não da posição do módulo na lista. Por exemplo, o pool é construído quando o consumidor pede o `Service`, então o hook do pool é sempre registrado antes e parado depois. Por isso o mutante que trocava a ordem dos módulos `postgres` e `sqs` foi removido como equivalente.
- **Como verificar:**
  - `TestCompositionLifecycle` (tag `integration`, `fxtest`): sobe a composição real sobre banco, filas e tópico exclusivos e abre uma carteira pela API; o worker publica os 2 eventos. Ao parar, confere que nenhum hook falhou, a ordem (workers, servidor HTTP, consumidor e, por fim, o pool) e a liberação de fato: o pool recusa conexões, a porta HTTP está livre e uma mensagem enviada depois do stop continua na fila.
  - `TestGracefulTermination` (tag `resilience`): SIGTERM em um processo real sob carga; o processo sai com código 0 dentro do `SHUTDOWN_TIMEOUT`, e as outras instâncias concluem tudo.
  - Mutantes "pool do banco nunca é fechado", "consumidor sem hook de stop", "servidor HTTP não para no stop" e "publisher da outbox sem hook de stop".
- **Limitação observada:** um long polling do SQS aberto antes do stop pode, no broker, ainda reservar uma mensagem depois que o cliente desistiu. Ela não se perde: volta a ficar visível quando vence a visibilidade (30 s em produção) e outra instância a processa. O teste espera o long polling (1 s) terminar antes de conferir a fila.
- **Consumidor SQS:** ligado ao ciclo de vida (`StartConsumer`), para antes do pool do banco e tem encerramento seguro (§4.14).
- **Workers:** o publisher da outbox e o worker de referências são ligados ao ciclo de vida em `internal/worker` e param antes do pool do banco, liberando as reservas não iniciadas (§4.15). Em `docker compose stop`, a ordem observada é: workers, servidor HTTP, consumidor SQS e, por fim, o pool (o Fx para os componentes na ordem inversa em que foram registrados).

### 4.17 Ambiente, ferramentas e testes

#### Toolchain sem instalação local

- **Contexto:** seção 2.1 do enunciado: o único pré-requisito do host é Docker com Compose.
- **Escolha:**
  - Serviço `tools` (perfil `tools` do Compose), construído sobre `golang:1.27.2-bookworm`, com `goose v3.29.0`. O CGO fica habilitado para `-race`.
  - O código-fonte é montado como volume, e os caches de módulos e de build ficam em volumes nomeados.
  - O container roda como `HOST_UID:HOST_GID` (padrão 1000), para que os arquivos gerados pertençam ao usuário.
- **Imagens fixadas:** Go `1.27.2`; runtime `gcr.io/distroless/static-debian13:nonroot` (sem shell, sem root); PostgreSQL `18.6-alpine`; Keycloak `26.8.1`; LocalStack `4.14.0`; Toxiproxy `2.12.0`.
- **Base das imagens:** Debian para a toolchain (glibc e gcc para `-race`) e distroless para o runtime.
  - Alpine exigiria musl e gcc extras para `-race`.
  - Debian slim no runtime aumentaria a superfície de ataque.

#### LocalStack `4.14.0`

- **Contexto:** "SQS FIFO (LocalStack ou MiniStack nos testes e no ambiente local)." / Execução reproduzível a partir de checkout limpo.
- **Opções consideradas:** (a) `4.14.0`, sem token; (b) `latest` com `LOCALSTACK_AUTH_TOKEN`; (c) MiniStack.
- **Prós e contras:**
  - (a) roda em qualquer máquina sem conta.
  - (b) foi verificada: a imagem `latest` (2026.10.0) encerra com "License activation failed" sem token, o que quebra a reprodução a partir de checkout limpo.
  - (c) exigiria validar FIFO, redrive e deduplicação.
- **Escolha e justificativa:** (a).
- **Critério impactado:** Documentação e Testes.
- **Como verificar:** `docker compose up --build` sobe o LocalStack *healthy*, com as filas criadas.
- **Limitações:** linha congelada, sem novas correções. Para outra versão, basta trocar a tag no `compose.yaml`; versões com licença exigem `LOCALSTACK_AUTH_TOKEN`.

#### D12. Ferramenta de migrations: `goose`

- **Contexto:** "Ferramenta de migrations (com aplicação e reversão)."
- **Opções consideradas:** (a) `goose`; (b) `golang-migrate`.
- **Prós e contras:**
  - (a) usa arquivos SQL com `Up` e `Down` no mesmo arquivo, CLI no container e pode ser usado como biblioteca nos testes.
  - (b) é muito difundido, mas usado como biblioteca nos testes puxa mais dependências de drivers.
- **Escolha e justificativa:** (a), `v3.29.0`, instalado na imagem `tools`.
- **Como verificar:** `TestMigrationsApplyRevertAndReapply` e os comandos `goose` do README.
- **Limitações:** a forma de aplicação está descrita no §4.11 (serviço one-shot).

#### D13. Build tags

- **Contexto:** "Uso ou não de build tags para separar testes de integração, multi-instância e falha."
- **Opções consideradas:** (a) tags `integration` e `resilience`; (b) só `integration`; (c) variável de ambiente com `t.Skip`.
- **Prós e contras:**
  - (a) faz `go test ./...` rodar só os testes unitários, sem infraestrutura, e separa os testes de falha, que são lentos.
  - (b) roda sempre os testes de falha junto com os de integração.
  - (c) compila e lista os testes de infra no comando padrão, e é fácil esquecer a variável.
- **Escolha e justificativa:** (a).
- **Como verificar:** `go test ./...` passa sem nenhum container de dependência no ar.
- **Limitações:** quem esquecer a tag não roda a integração; o README traz os comandos completos.

#### D15. Testes de integração a partir do container de toolchain

- **Contexto:** "Como os testes de integração, multi-instância e falha controlam containers a partir do container de toolchain […], incluindo o impacto em segurança e em reprodutibilidade."
- **Opções consideradas:** (a) dependências declaradas no Compose, com as instâncias da aplicação rodando como processos independentes dentro do container `tools`; (b) Testcontainers com `/var/run/docker.sock` montado.
- **Prós e contras:**
  - (a) não precisa do socket do Docker, então não dá acesso equivalente a root no host. A queda de processo é simulada com `SIGKILL`, e a indisponibilidade de PostgreSQL e SQS com o Toxiproxy.
  - (b) é mais flexível e isolado por teste, mas o socket montado equivale a root no host e a execução fica mais lenta.
- **Escolha e justificativa:** (a). Cada instância é um processo com suas próprias conexões e memória, o que atende "pelo menos três processos independentes".
- **Critério impactado:** Testes e Concorrência.
- **Como verificar:** `test/resilience` (§4.19): o teste compila o binário e sobe 3 processos filhos no próprio container `tools`; o Toxiproxy é controlado pela sua API HTTP; nenhum socket do Docker é montado.
- **Limitações:** os testes dependem de a stack do Compose estar no ar; o README traz o comando.

#### Testes de mutação curados

- **Contexto:** os testes precisam demonstrar as garantias, não apenas passar.
- **Opções consideradas:** (a) script curado no repositório; (b) ferramenta automática (gremlins); (c) as duas; (d) não versionar.
- **Prós e contras:**
  - (a) é determinístico e mostra qual teste mata cada defeito.
  - (b) dá cobertura ampla, mas é lenta e ruidosa (mutantes equivalentes).
  - (d) não deixaria evidência reproduzível.
- **Escolha e justificativa:** (a). O `scripts/mutation.sh` injeta cada defeito numa cópia descartável do código. O resultado é `KILLED`, `SURVIVED`, `INVALID` (mutante que não compila, que não prova nada) ou `ERROR`, e o script falha em qualquer resultado diferente de `KILLED`. O script cresce a cada fase.
- **Critério impactado:** Testes.
- **Como verificar:** o comando abaixo, que hoje dá 122 mutantes mortos e 0 problemas: 28 de domínio, 5 de logs e métricas, 20 de persistência, 23 de casos de uso, 16 de HTTP e autenticação, 12 do consumidor SQS, 14 da outbox e dos workers e 4 da composição Fx (os seis últimos grupos com PostgreSQL, Keycloak e LocalStack no ar). Com `MUTATION_RESILIENCE=1`, entram mais 4 mutantes contra os cenários de 3 processos (126 no total; alguns minutos a mais). `MUTATION_FILTER` roda só os mutantes cujo nome casar com uma expressão regular.
```bash
docker compose run --rm tools bash scripts/mutation.sh
```
- **Mutantes equivalentes:** um mutante que não muda nenhum comportamento observável não é mantido no script; o motivo é registrado aqui. "Falha não segura as seguintes do mesmo grupo" foi removido: a ordem por carteira é garantida pelo próprio SQS FIFO (nenhuma mensagem de um grupo é entregue enquanto outra está em voo), e reagendar as mensagens seguintes só reduz a espera delas. O mutante revelou que o comentário do código dava o motivo errado, e o comentário foi corrigido. "Marcação ignora o dono da reserva" (outbox) também foi removido: a marcação exige `published_at IS NULL`, então nunca há dupla marcação, com ou sem a checagem do dono; essa checagem só permite registrar no log que o lease foi assumido por outro publisher.
- **Mutantes sobre comportamento aleatório:** o jitter do backoff faz o resultado de um teste variar entre execuções. Três rodadas seguidas mostraram que o mutante "backoff sem teto" às vezes sobrevivia: sem o teto, a espera seria 64s ±20%, quase toda sobreposta à faixa correta de 60s ±20%, e uma única amostra não bastava. `TestReferencePolicyCapsDelay` passou a amostrar 500 vezes e exigir que nenhuma espera passe de 72s; o mutante é morto de forma estável. Por isso, mudanças em código com aleatoriedade devem rodar o script mais de uma vez.
- **Limitações:** só cobre os defeitos listados; não substitui revisão.

#### Configuração e logs

- **Configuração:** variáveis de ambiente lidas com a stdlib (`os.LookupEnv`), parse e validação explícitos. A alternativa (`caarlos0/env`) foi descartada para não ter dependência extra nem validação implícita. As credenciais da AWS não entram em `Config`: o SDK as lê direto do ambiente.
- **Logs:** `log/slog` com `JSONHandler`, sem dependência extra. `zap` foi descartado por não trazer ganho necessário neste escopo. Atributos de contexto, access log e regras de conteúdo estão no §4.18.
- **Readiness:** os detalhes de erro vão para o log, nunca para a resposta pública.

### 4.18 Observabilidade

Todas as escolhas desta seção foram feitas explicitamente, uma a uma.

#### Biblioteca e exposição das métricas

- **Contexto:** "Logs estruturados com IDs relevantes, métricas de operações, latência, duplicatas, retries, DLQ, conflitos de concorrência, atraso da outbox e divergência de reconciliação."
- **Opções consideradas:** (a) `prometheus/client_golang`; (b) OpenTelemetry com exportador Prometheus; (c) `expvar` da stdlib.
- **Prós e contras:**
  - (a) é o padrão de fato para o formato Prometheus, com histogramas e registry testável.
  - (b) é neutro de fornecedor, porém traz mais dependências e configuração sem ganho neste escopo.
  - (c) não tem labels nem histogramas.
- **Escolha e justificativa:** (a). As métricas ficam em `GET /metrics` numa **porta separada** (`METRICS_ADDR`, padrão `:9090`), não publicada no host: só a rede interna as alcança. Assim, a superfície pública continua sendo só a API autenticada, e a coleta não depende de token. Os coletores padrão do runtime Go e do processo são registrados.
- **Critério impactado:** Observabilidade.
- **Como verificar:** com a stack no ar, `docker compose run --rm tools curl -s http://app:9090/metrics` lista as séries `wallet_*`, `go_*` e `process_*`; `curl localhost:9090` no host não conecta.
- **Limitações:** não há Prometheus nem dashboards no Compose; a coleta fica a cargo do ambiente.

#### Porta de métricas na camada de aplicação

- **Opções consideradas:** (a) interface `app.Metrics`, com implementação Prometheus em `internal/platform/metrics` e `NopMetrics` para quem não mede; (b) chamar o Prometheus direto nos casos de uso.
- **Escolha e justificativa:** (a). A camada `app` registra fatos ("operação concluída", "retry por versão desatualizada") sem conhecer a biblioteca, mantendo a regra de dependência para dentro. Cada teste cria o seu próprio registry, então as asserções não interferem umas nas outras.
- **Como verificar:** `TestCatalog` (cada método alimenta a série certa) e os testes de integração com registry próprio listados abaixo.

#### Catálogo

| Métrica | Tipo | Labels |
|---|---|---|
| `wallet_operations_total` | counter | `channel`, `kind`, `status`, `failure_code` |
| `wallet_operation_duration_seconds` | histograma (buckets padrão) | `channel`, `kind`, `outcome` |
| `wallet_duplicates_total` | counter | `channel`, `reason` (`idempotent_replay`, `inbox_redelivery`) |
| `wallet_retries_total` | counter | `component` (`usecase`, `sqs_consumer`, `outbox_publisher`), `reason` |
| `wallet_dead_letters_total` | counter | `queue`, `error_code` |
| `wallet_dead_letter_queue_messages` | gauge | `queue` |
| `wallet_concurrency_conflicts_total` | counter | `kind` (`stale_version`, `lock_timeout`, `deadlock`, `serialization`, `unique`) |
| `wallet_outbox_publish_delay_seconds` | histograma (padrão + 30 s, 60 s, 5 min, 15 min) | — |
| `wallet_outbox_pending_events`, `wallet_outbox_oldest_pending_age_seconds` | gauges | — |
| `wallet_reconciliations_total` | counter | `result` (`consistent`, `divergent`) |
| `wallet_pending_reference_attempts_total` | counter | `result` (`waiting`, `completed`, `expired`) |

- **Sem label de provedor ou carteira:** ids teriam cardinalidade sem limite. O provedor aparece nos logs. `kind` inválido vira `unknown`, pelo mesmo motivo.
- **DLQ:** o counter mede o que o consumidor envia (por código); o gauge mede a profundidade real das duas DLQs (entrada e eventos), incluindo o que chegou pelo redrive do SQS ou da assinatura, que o processo não vê.
- **Buckets:** os padrões cobrem a latência das operações; o atraso da outbox ganhou buckets longos porque o backoff do publisher vai até 5 min.

#### O que conta como operação, duplicata e erro

- **Escolha:**
  - replays e reentregas contam **só** em `wallet_duplicates_total`; `wallet_operations_total` conta cada operação uma única vez;
  - erros (validação, conflito, indisponibilidade) entram **só** no histograma, com `outcome` igual ao código estável da API, como `AMOUNT_MUST_BE_POSITIVE` ou `UNAVAILABLE`; rejeições persistidas são operações (`status=REJECTED` com `failure_code`);
  - a abertura com saldo positivo conta como `OPENING`; as conclusões e expirações de pendências contam no canal `worker`.
- **Latência:** medida no caso de uso (mesma régua para HTTP, SQS e worker), sem o tempo de rede e de autenticação.
- **Como verificar:** `TestMetricsFiftyIdenticalBets` (50 apostas iguais: 1 operação, 49 duplicatas, 50 amostras), `TestMetricsRejectionAndErrors`, `TestMetricsMessageChannel`, `TestMetricsPendingReferenceExpiration`.

#### Conflitos e retries

- **Conflitos** são contados no adaptador de banco, que é quem conhece o SQLSTATE: `40001` → `serialization`, `40P01` → `deadlock`, `55P03` → `lock_timeout`, versão desatualizada → `stale_version` e unicidade → `unique`. A contagem acontece ao fim de cada transação da UnitOfWork.
- **Retries** são contados por quem repete: o caso de uso (pelo motivo da falha anterior), o consumidor SQS (falha transitória) e o publisher (erro do broker).
- **Como verificar:** `TestSaveDetectsStaleVersion`, `TestLockTimeoutIsTransient`, `TestIdempotencyConstraints`, `TestMetricsUseCaseRetries` (UnitOfWork que falha uma vez por versão e uma por indisponibilidade antes de delegar), `TestTransientFailureIsRetried`, `TestBrokerUnavailableIsRetriedWithBackoff`.

#### Dinheiro fora das métricas

- **Contexto:** o enunciado pede métrica de divergência de reconciliação e proíbe `float32`/`float64` para dinheiro; as métricas Prometheus são `float64`.
- **Opções consideradas:** (a) gauge com o valor da divergência; (b) só a contagem de reconciliações por resultado.
- **Escolha e justificativa:** (b). O valor da divergência continua na resposta da reconciliação e no log de `error`. O teste de ausência de ponto flutuante ignora apenas o pacote `internal/platform/metrics`, e `TestMetricsNeverSeeMoney` garante que esse pacote não importa `money`, `wallet` nem `wagering`.
- **Limitações:** um alerta de divergência informa que ela existe, não o tamanho; o tamanho está no log.

#### Atualização dos gauges

- Um loop (`gauge-refresher`) lê a cada `METRICS_REFRESH_INTERVAL` (padrão 15 s) o backlog da outbox, numa transação somente leitura, e a profundidade das DLQs (mensagens visíveis + em voo). Só roda com as métricas ligadas. Uma fila inexistente é logada em `warn` e não impede a leitura das demais.
- **Como verificar:** `TestGaugeLoopReportsBacklogAndDeadLetters`.
- **Limitações:** com várias instâncias, cada uma publica o mesmo gauge global; a agregação deve usar `max`, não soma.

#### Logs

- **Atributos de contexto:** o pacote `logctx` guarda atributos no `context.Context` (`correlationId`, `clientId`, `providerId`, `walletId`, `messageId`, `transactionId`), e um handler os acrescenta a cada linha. Uma chave repetida substitui a anterior, e o contexto pai não é alterado.
- **Access log:** uma linha `info` por requisição com método, **padrão da rota** (nunca a URL com ids), status, duração, `clientId` e `providerId`. Health checks vão para `debug`, e rotas inexistentes aparecem como `unmatched`.
- **Conteúdo:** só ids e códigos. Tokens, `Authorization`, segredos e valores monetários nunca entram; a exceção é o log de divergência de reconciliação, cujo valor é o diagnóstico.
- **Como verificar:** `TestContextAttributesReachEveryLine`, `TestContextIsNotMutated`, `TestAccessLog`, `TestLogsCarryNoCredentialsOrMoney` (API, inclusive com token inválido e JSON quebrado) e `TestLogsCarryIdsButNoMoney` (casos de uso).
- **Correção nesta fase:** o worker de referências (Fase 8) registrava `component` duas vezes em cada linha; o logger com o componente era passado ao loop, que o acrescentava de novo. Corrigido, junto com o mesmo padrão no `gauge-refresher`; `TestGaugeLoopReportsBacklogAndDeadLetters` confere uma única ocorrência.

### 4.19 Testes de concorrência, múltiplas instâncias e falha

Todas as escolhas desta seção foram feitas explicitamente, uma a uma. A base é a D15 (§4.17): dependências no Compose e instâncias da aplicação como processos independentes dentro do container `tools`, sem o socket do Docker. Os testes ficam em `test/resilience`, com a tag `resilience` (D13).

#### Como as instâncias sobem

- **Contexto:** "Garantias demonstradas com pelo menos três processos independentes, cada um com suas próprias conexões e memória." / "Cenários relevantes repetidos com ≥ 3 instâncias independentes."
- **Opções consideradas:** (a) o `TestMain` compila `cmd/wallet` e cada instância é um processo filho (`exec.Command`); (b) um script shell sobe os processos e os testes só fazem chamadas.
- **Prós e contras:**
  - (a) dá ao teste controle preciso de SIGKILL, SIGTERM e reinício no momento certo, e acesso aos logs de cada processo.
  - (b) é mais simples de ler, mas o controle de queda fica fora do teste e é mais frágil.
- **Escolha e justificativa:** (a), com estes parâmetros:
  - **3 instâncias** por cenário (o mínimo exigido), cada uma com portas HTTP e de métricas próprias;
  - binário compilado com **`-race`**: uma condição de corrida em qualquer processo derruba o processo e falha o teste;
  - banco temporário (`pgtest`), filas e tópico **exclusivos de cada teste**, sem interferir no app do Compose nem entre cenários;
  - pacote `test/resilience`, fora de `internal/`: o teste é caixa-preta (HTTP, SQS e consultas ao banco);
  - **tempos encurtados** só por variáveis de ambiente (visibilidade 5 s, long polling 1 s, backoff do consumidor 1–4 s, lease 3 s, worker de referências a cada 200 ms, `SHUTDOWN_TIMEOUT` 5 s); o código é o de produção;
  - **log por instância** em arquivo; ao fim, cada log é varrido em busca de `panic` e `DATA RACE`, e, se o teste falhou, as últimas linhas de cada instância são impressas;
  - **métricas por instância**, somadas pelo teste como evidência (duplicatas, reentregas e conflitos entre processos).
- **Rede:** as instâncias alcançam o PostgreSQL e o LocalStack (SQS e SNS) pelo Toxiproxy; o próprio teste fala direto com eles, para poder enviar mensagens e conferir o banco durante uma queda simulada. O Toxiproxy é controlado pela sua API HTTP com `net/http`, sem dependência nova.
- **Critério impactado:** Concorrência, Idempotência, Mensageria e recuperação, e Testes.
- **Como verificar:** `docker compose run --rm tools go test -tags=resilience -v ./test/resilience/` (2 a 3 minutos, incluindo a compilação com `-race`).
- **Limitações:** as instâncias rodam no mesmo container, então compartilham CPU e rede, mas não memória nem conexões.

#### Cenários

| Teste | Cenário | O que comprova |
|---|---|---|
| `TestFiftyIdenticalBetsAcrossInstances` | a mesma aposta 50 vezes em paralelo, espalhada pelos 3 processos | 1 débito, 49 replays (também na soma das métricas) e nenhum conflito de versão |
| `TestTwoBetsOf80AcrossInstances` | 2 apostas de 80.00 numa carteira de 100.00, em processos diferentes, em 3 rodadas | 1 `PROCESSED`, 1 `REJECTED` (`INSUFFICIENT_FUNDS`), saldo 20.00, 1 débito; reenvios não mudam o resultado |
| `TestBurstOnOneWalletAcrossInstances` | 60 apostas distintas na mesma carteira, disparadas juntas | todas aplicadas, saldo 40.00 e **zero** conflitos de concorrência: o lock serializa os processos |
| `TestIndependentWalletsAcrossInstances` | 15 carteiras × 10 apostas, pelos 3 processos | todas as carteiras com o saldo esperado |
| `TestHTTPAndSQSAcrossInstances` | a mesma operação por HTTP e por SQS (duas mensagens com a mesma chave e uma cópia da primeira) | 1 débito por operação; 3 duplicatas por operação, das quais 1 barrada pela inbox |
| `TestWorkersAcrossInstances` | 3 publishers na mesma outbox; reversões antes das apostas, resolvidas pelos 3 workers | cada evento entregue uma vez e na ordem de cada carteira; cada reversão concluída uma vez |
| `TestKillDuringLoad` | SIGKILL em uma instância no meio de uma rajada HTTP + SQS; ela volta 2 s depois | 1 débito por operação, nenhuma mensagem perdida, DLQ vazia |
| `TestRestartAllInstances` | as 3 instâncias caem juntas com operações concluídas, uma rejeição, reversões pendentes e eventos não publicados; sobem 3 novas | replays devolvem o resultado original; pendências retomadas; outbox drenada |
| `TestKillConsumerAfterCommit` | SIGKILL entre o commit e a remoção da mensagem | a reentrega é barrada pela inbox (métrica `inbox_redelivery` > 0) e a operação continua aplicada uma vez |
| `TestKillPublisherHoldingClaims` | SIGKILL no único publisher enquanto ele tem eventos reservados | outro publisher assume depois do lease; cada evento chega uma vez, com o mesmo `eventId`, na ordem da carteira |
| `TestGracefulTermination` | SIGTERM em uma instância sob carga | saída com código 0 dentro do prazo; nada se perde |
| `TestDatabaseOutage` | PostgreSQL inalcançável por 5 s no meio da carga | `503 UNAVAILABLE` sem efeito parcial; readiness falha e volta; mensagens voltam à fila sem ir para a DLQ; tudo aplicado uma vez |
| `TestBrokerOutage` | SQS e SNS inalcançáveis por 6 s | a API continua operando; readiness falha; eventos esperam na outbox com tentativas registradas; fila e outbox drenadas ao voltar |

Todos os cenários terminam com a mesma verificação: o saldo de cada carteira bate com o ledger, nenhuma transação tem mais de um lançamento, a outbox está toda publicada e cada evento chegou exatamente uma vez à fila assinante.

#### Momento da queda: carga e evidência

- **Contexto:** quedas "após o commit e antes da remoção" e "com eventos reservados" precisam acontecer de fato, não só em teoria.
- **Opções consideradas:** (a) matar sob carga, sem código de teste no binário, e exigir evidência de que a janela foi atingida; (b) ganchos por variável de ambiente que pausam o binário no ponto exato; (c) matar sob carga sem exigir evidência.
- **Escolha e justificativa:** (a). O ponto exato já é coberto de forma determinística pelos ganchos dos testes de integração (Fases 7 e 8). Aqui, cada cenário de queda **recusa o próprio resultado** se não houver evidência:
  - a queda durante a carga precisa acontecer antes de 3/4 da carga (nas execuções observadas, entre 97 e 112 de 400 débitos);
  - a queda do publisher precisa deixar eventos reservados e ainda com lease (nas execuções observadas, entre 43 e 50);
  - a reinicialização precisa encontrar eventos não publicados (30);
  - a queda do consumidor precisa produzir uma reentrega barrada pela inbox.
- **Janela entre o commit e a remoção:** é estreita demais para ser acertada de forma estável só com carga. Atrasar as requisições ao SQS não alargou a janela, porque o Toxiproxy entrega o `DeleteMessage` já recebido mesmo depois que o processo morre. A solução adotada é uma **latência de 100 ms nas respostas do PostgreSQL**, só nesse teste: o `COMMIT` é aplicado no banco e a confirmação chega atrasada, então um SIGKILL nesse intervalo produz exatamente o caso real. Com isso, a janela é atingida na primeira rodada em todas as execuções observadas.
- **Limitações:** a evidência é estatística, não o ponto exato. O teste tenta até 6 rodadas antes de falhar.

#### Indisponibilidade com o Toxiproxy

- **Escolha:** PostgreSQL fora (5 s) e SQS/SNS fora (6 s), desligando o proxy, que recusa conexões novas e derruba as abertas. A latência alta no banco como cenário próprio não foi incluída.
- **Ajuste de tempos:** o backoff do consumidor (1–4 s) faz 5 recebimentos durarem mais que a queda de 5 s, então nenhuma mensagem vai para a DLQ só por causa da queda. O mutante "queda do banco manda mensagens para a DLQ" prova que o teste percebe o contrário.

#### Mutantes contra os processos reais

- **Escolha:** um grupo rápido da composição Fx (4 mutantes, sempre) e 4 mutantes de resiliência, cada um rodando só o cenário que o detecta, ativados por `MUTATION_RESILIENCE=1`, porque cada um recompila o binário e leva alguns minutos.
- **O que eles mostraram:**
  - "lock da carteira sem FOR UPDATE" sobrevivia aos cenários de apostas: duas requisições raramente se sobrepõem no tempo, e a checagem de versão, segunda defesa da D1, mantinha o resultado correto. Isso levou ao `TestBurstOnOneWalletAcrossInstances`, que exige zero conflitos. Sem o lock, a colisão aparece como conflito de **unicidade na cadeia do ledger** (a mesma versão gravada duas vezes) antes de chegar à checagem de versão da carteira; por isso o cenário exige zero conflitos de qualquer tipo.
  - "queda do banco manda mensagens para a DLQ" só morria por timeout; a espera passou a falhar assim que aparece mensagem na DLQ.

---

## 5. Evidências por critério de avaliação

| Critério | Pontos | Evidência atual | Pendente |
|---|---|---|---|
| Integridade financeira | 20 | Money exato e sem float (`TestNoFloatingPointInProductionCode`); invariantes da Wallet e do ledger; regras dos 5 tipos; reversões (§4.1, §4.4, §4.8). No banco: saldo ≥ 0, ledger append-only (privilégio + trigger), cadeia do ledger e conferência saldo × ledger no commit, máquina de estados e regras dos tipos (§4.11); reconciliação em snapshot único que detecta divergência sem alterar o saldo (`TestReconciliation`, §4.12) | — |
| Concorrência | 20 | `FOR UPDATE` por carteira + versão no UPDATE (D1); `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100`, `TestIndependentWalletsDoNotBlock`, `TestLockTimeoutIsTransient` (§4.11); `TestFiftyIdenticalBetsInParallel`, `TestTwoBetsOf80OnWalletWith100`, `TestIndependentWalletsInParallel` (§4.12); com 3 processos independentes: 50 apostas iguais, 2 × 80.00, rajada na mesma carteira sem nenhum conflito e carteiras independentes (§4.19) | — |
| Idempotência | 15 | hash canônico versionado; `CheckReplay` (§4.7); unicidade por provedor e por operação no banco (`TestIdempotencyConstraints`); fluxo de replay com o resultado original, conflitos e chave não consumida por erro corrigível (§4.12); HTTP × SQS nas duas ordens e reentregas deduplicadas pela inbox (§4.14); HTTP × SQS entre 3 processos e replays com o resultado original depois do reinício de todos os processos (§4.19) | — |
| Mensageria e recuperação | 15 | máquina de estados e regras de espera (§4.5, §4.8); tabelas de inbox e outbox com deduplicação e snapshot imutável (§4.11); espera durável por referência com backoff, expiração e resolução única entre instâncias; retry de falhas transitórias (§4.12); consumidor com inbox na mesma transação, DLQ imediata para erros permanentes, backoff e redrive, queda entre commit e remoção, heartbeat e encerramento seguro (§4.14); publisher da outbox com reserva por lease, ordem por agregado, backoff sem desistir, recuperação de trabalho abandonado, republicação com o mesmo eventId e DLQ da assinatura; worker de referências paralelo por carteira (§4.15); SIGKILL sob carga, entre o commit e a remoção, com eventos reservados e de todas as instâncias; PostgreSQL e SQS/SNS fora do ar com o Toxiproxy (§4.19) | — |
| Modelagem e arquitetura | 10 | domínio isolado (`TestDomainHasNoInfrastructureImports`); erros tipados; UnitOfWork e repositórios (D11); módulos Fx com raiz de composição testada (`TestCompositionLifecycle`, §4.16); autenticação OIDC, scopes por operação e isolamento por provedor (§4.13) | — |
| Testes | 10 | testes unitários e de integração (repositórios, casos de uso, API com Keycloak real, consumidor e publisher com LocalStack real) com PostgreSQL real, todos com `-race`; 122/122 mutantes mortos: 28 de domínio, 5 de logs e métricas, 20 de SQL e repositórios, 23 de casos de uso, 16 de HTTP e autenticação, 12 do consumidor, 14 da outbox e dos workers, 4 da composição Fx, mais 4 opcionais contra os 3 processos (§4.17); 13 cenários com 3 processos reais compilados com `-race`, SIGKILL, SIGTERM e Toxiproxy (§4.19) | — |
| Observabilidade | 5 | logs JSON com ids pelo contexto e access log, sem credenciais nem valores; métricas Prometheus de operações, latência, duplicatas, retries, DLQ, conflitos, atraso e backlog da outbox, reconciliação e pendências, em porta interna; `/health/live`; `/health/ready` com PostgreSQL e SQS (§4.18) | — |
| Documentação | 5 | este documento e o README | revisão final |

---

## 6. Checklist de garantias obrigatórias

| Garantia | Estado | Evidência |
|---|---|---|
| Dinheiro nunca passa por `float32`/`float64` | ✅ | `TestNoFloatingPointInProductionCode`; parsing estrito; JSON numérico rejeitado |
| Idempotência persistente, sobrevive ao reinício | ✅ | chave, hash e resultado persistidos com unicidade no banco; replay lido do banco (§4.11, §4.12); `TestRestartAllInstances`: depois de derrubar os 3 processos, replays em processos novos devolvem o resultado original (§4.19) |
| Invariantes financeiras garantidas no banco | ✅ | §4.11: CHECKs, triggers de cadeia, de versão e de estado, conferência diferida saldo × ledger; `TestBalanceChangeRequiresLedgerEntry`, `TestDatabaseEnforcesDomainRules` |
| Eventos externos publicados só após o commit | ✅ | outbox gravada no mesmo commit (`TestUnitOfWorkRollsBackEverything`); o publisher não enxerga transações abertas (`TestNothingIsPublishedBeforeCommit`) |
| Ledger append-only; correções por novos lançamentos | ✅ | privilégio só `SELECT`/`INSERT` para a aplicação e triggers contra `UPDATE`/`DELETE`/`TRUNCATE`; `TestLedgerIsAppendOnly` |
| Carteiras independentes em paralelo; nenhum lock global | ✅ | lock só na linha da carteira; `TestIndependentWalletsDoNotBlock`; `TestIndependentWalletsAcrossInstances` (3 processos) |
| Sem lost updates no saldo | ✅ | `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100`; com 3 processos, `TestBurstOnOneWalletAcrossInstances` e `TestTwoBetsOf80AcrossInstances` |
| Unicidade, não negatividade e imutabilidade impostas pelo banco | ✅ | índices únicos, `CHECK (balance_minor >= 0)` e triggers (§4.11) |
| Sem duplicidade, saldo negativo ou perda de evento nas situações listadas | ✅ | Recebimento repetido por HTTP e SQS: `TestHTTPAndSQSShareIdempotency`, `TestHTTPAndSQSAcrossInstances`. Reversão antes da referência: `TestReversalBeforeReferenceResolvedByWorker`, `TestWorkersAcrossInstances`. Processamento simultâneo na mesma carteira: `TestBurstOnOneWalletAcrossInstances`. Encerramento abrupto antes ou depois de um commit: `TestKillDuringLoad`, `TestKillConsumerAfterCommit`, `TestRestartAllInstances`. Publicação repetida de evento: `TestRepublishAfterCrashKeepsEventID`, `TestKillPublisherHoldingClaims`. Indisponibilidade temporária: `TestDatabaseOutage`, `TestBrokerOutage` (§4.19) |

---

## 7. Limitações e trabalho não concluído

- A divergência de reconciliação é contada nas métricas, mas o valor fica só na resposta e no log (§4.18).
- Os gauges globais (backlog da outbox, profundidade das DLQs) são publicados por todas as instâncias; a agregação deve usar `max` (§4.18).
- Não há Prometheus nem dashboards no Compose; as métricas ficam disponíveis para coleta na rede interna.
- A deduplicação de eventos pelo broker vale por 5 minutos; depois disso, cabe ao consumidor deduplicar pelo `eventId` (§4.15).
- Todas as decisões D1–D15 estão tomadas.
- O LocalStack comunitário não aplica as políticas de acesso das filas (§4.14).
- Os testes de resiliência rodam as 3 instâncias no mesmo container: são processos independentes (memória e conexões próprias), mas compartilham CPU e rede (§4.19).
- A evidência de queda na janela entre o commit e a remoção é estatística; o ponto exato é coberto pelos ganchos dos testes de integração (§4.14, §4.19).
- Não há teste multi-instância dedicado a duas reversões simultâneas da mesma aposta; a regra é garantida pelo índice único `wager_transactions_one_reversal_uq` (§4.8).
- A expiração de referência pendente é testada com um processo; com 3 processos, só a resolução é testada. As duas conclusões passam pelo mesmo lock da carteira e pela mesma checagem do agendamento (§4.12, §4.19).
- Depois de um stop, um long polling já aberto pode reservar uma mensagem no broker; ela volta após a visibilidade e é processada por outra instância (§4.16).
- O banco não confere que a transação está `PROCESSED` quando o seu lançamento é gravado (§4.11).
- Moedas limitadas a BRL, USD e EUR, todas com 2 casas decimais.
- LocalStack fixado na linha comunitária `4.14.0`, que não recebe novas correções.
