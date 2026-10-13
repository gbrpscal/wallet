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
| Consumidor SQS | Lê `wager-transactions.fifo` e usa o mesmo caso de uso da API. *(pendente)* |
| Worker de referências | Retoma operações em `PENDING_REFERENCE` com backoff. *(pendente)* |
| Publisher da outbox | Publica os eventos confirmados no banco. *(pendente)* |
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
internal/app/                portas (UnitOfWork, repositórios), erros de infraestrutura;
                             casos de uso (pendente)
internal/adapters/
  postgres/                  pool, UnitOfWork, repositórios e classificação de erros
  httpapi/                   servidor, rotas e health checks
  sqs/                       cliente, consumidor e publisher
internal/platform/
  config/  logging/  health/ configuração, logs JSON e readiness
migrations/                  schema, constraints e triggers (goose)
deploy/                      papéis do PostgreSQL, realm do Keycloak, filas, Toxiproxy
scripts/mutation.sh          testes de mutação curados
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
| 5 | Casos de uso | pendente |
| 6 | HTTP e autenticação | pendente |
| 7 | Consumidor SQS | pendente |
| 8 | Outbox e workers | pendente |
| 9 | Observabilidade | pendente |
| 10 | Testes de concorrência, multi-instância e falha | pendente |
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
- **Como verificar:** `TestReversalRejections/already_reversed` (domínio) e `TestOneSuccessfulReversalPerReference` (banco). A corrida entre reversões em processos diferentes será testada na Fase 10.
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
- **Limitações:** os parâmetros de espera (D4) estão pendentes.

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
- **Limitações:** uma carteira muito disputada tem o throughput limitado à serialização dos seus escritores. Por enquanto só há testes com um processo; a prova com ≥ 3 processos fica para a Fase 10.

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

### 4.12 Decisões pendentes

| # | Decisão | Fase |
|---|---|---|
| D4 | Tentativas, TTL e backoff de `PENDING_REFERENCE` | 5 |
| D14 | Aceite síncrono ou assíncrono | 5 |
| D6 | Mapeamento de códigos para HTTP | 5/6 |
| D8 | Validação de token, permissões e identificação do provedor | 6 |
| D9 | `MessageGroupId`, deduplicação, visibility timeout, `maxReceiveCount`, mensagens inválidas | 7 |
| D10 | Destino dos eventos e contrato de roteamento | 8 |

### 4.13 Composição com Fx e ciclo de vida (parcial)

- **Contexto:** "Fx compõe configuração, conexões, repositórios, casos de uso, handlers e workers […] fechamento das dependências depois dos componentes que as utilizam."
- **Implementado até aqui:**
  - A configuração é carregada e validada **antes** do Fx. Uma configuração inválida encerra o processo com código 1, antes de abrir qualquer conexão.
  - Módulos `logging`, `postgres`, `sqs`, `app` e `httpapi` (`fx.Module` / `fx.Provide` / `fx.Invoke`), com logs do Fx em JSON via `fxevent.SlogLogger`.
  - As checagens de readiness são reunidas por um *value group* (`group:"readiness"`), e cada adaptador contribui com a sua.
  - O servidor HTTP abre o listener no `OnStart`, de modo que uma porta ocupada faz a inicialização falhar.
  - No `OnStop`, a readiness passa a falhar e o servidor drena as requisições dentro de `SHUTDOWN_TIMEOUT` (`fx.StopTimeout`).
  - O pool do PostgreSQL é fechado depois do servidor (ordem inversa de inicialização do Fx).
- **Como verificar:** hoje, de forma manual: `docker compose stop app` mostra no log o hook do servidor executando antes do hook do pool. O teste automatizado da composição Fx será feito na Fase 10.
- **Pendente:** workers (consumidor, publisher, referências) e o shutdown seguro do consumidor SQS.

### 4.14 Ambiente, ferramentas e testes

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
- **Como verificar:** na Fase 10.
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
- **Como verificar:** o comando abaixo, que hoje dá 45 mutantes mortos e 0 problemas (28 de domínio e 17 de persistência, estes com o PostgreSQL no ar).
```bash
docker compose run --rm tools bash scripts/mutation.sh
```
- **Limitações:** só cobre os defeitos listados; não substitui revisão.

#### Configuração e logs

- **Configuração:** variáveis de ambiente lidas com a stdlib (`os.LookupEnv`), parse e validação explícitos. A alternativa (`caarlos0/env`) foi descartada para não ter dependência extra nem validação implícita. As credenciais da AWS não entram em `Config`: o SDK as lê direto do ambiente.
- **Logs:** `log/slog` com `JSONHandler`, sem dependência extra. `zap` foi descartado por não trazer ganho necessário neste escopo.
- **Readiness:** os detalhes de erro vão para o log, nunca para a resposta pública.

---

## 5. Evidências por critério de avaliação

| Critério | Pontos | Evidência atual | Pendente |
|---|---|---|---|
| Integridade financeira | 20 | Money exato e sem float (`TestNoFloatingPointInProductionCode`); invariantes da Wallet e do ledger; regras dos 5 tipos; reversões (§4.1, §4.4, §4.8). No banco: saldo ≥ 0, ledger append-only (privilégio + trigger), cadeia do ledger e conferência saldo × ledger no commit, máquina de estados e regras dos tipos (§4.11) | reconciliação |
| Concorrência | 20 | `FOR UPDATE` por carteira + versão no UPDATE (D1); `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100`, `TestIndependentWalletsDoNotBlock`, `TestLockTimeoutIsTransient` (§4.11) | testes com ≥ 3 processos |
| Idempotência | 15 | hash canônico versionado; `CheckReplay` (§4.7); unicidade por provedor e por operação no banco (`TestIdempotencyConstraints`) | fluxo de replay nos casos de uso; HTTP × SQS |
| Mensageria e recuperação | 15 | máquina de estados e regras de espera (§4.5, §4.8); tabelas de inbox e outbox com deduplicação e snapshot imutável (§4.11) | consumidor, publisher, retries, DLQ, shutdown |
| Modelagem e arquitetura | 10 | domínio isolado (`TestDomainHasNoInfrastructureImports`); erros tipados; UnitOfWork e repositórios (D11); módulos Fx (§1.2, §4.3, §4.13) | autenticação e autorização |
| Testes | 10 | testes unitários e 24 testes de integração com PostgreSQL real, todos com `-race`; 45/45 mutantes mortos, incluindo 17 de SQL e repositórios (§4.14) | isolamento entre provedores na API, paralelismo multiprocesso, interrupção |
| Observabilidade | 5 | logs JSON; `/health/live`; `/health/ready` com PostgreSQL e SQS | IDs nos logs; métricas |
| Documentação | 5 | este documento e o README | revisão final |

---

## 6. Checklist de garantias obrigatórias

| Garantia | Estado | Evidência |
|---|---|---|
| Dinheiro nunca passa por `float32`/`float64` | ✅ | `TestNoFloatingPointInProductionCode`; parsing estrito; JSON numérico rejeitado |
| Idempotência persistente, sobrevive ao reinício | parcial | chave, hash e resultado persistidos com unicidade no banco (§4.11); fluxo de replay e reinício nas Fases 5 e 10 |
| Invariantes financeiras garantidas no banco | ✅ | §4.11: CHECKs, triggers de cadeia, de versão e de estado, conferência diferida saldo × ledger; `TestBalanceChangeRequiresLedgerEntry`, `TestDatabaseEnforcesDomainRules` |
| Eventos externos publicados só após o commit | pendente | Fase 8 (a outbox já é gravada no mesmo commit: `TestUnitOfWorkRollsBackEverything`) |
| Ledger append-only; correções por novos lançamentos | ✅ | privilégio só `SELECT`/`INSERT` para a aplicação e triggers contra `UPDATE`/`DELETE`/`TRUNCATE`; `TestLedgerIsAppendOnly` |
| Carteiras independentes em paralelo; nenhum lock global | ✅ (um processo) | lock só na linha da carteira; `TestIndependentWalletsDoNotBlock`. Multiprocesso na Fase 10 |
| Sem lost updates no saldo | ✅ (um processo) | `TestConcurrentDebitsDoNotLoseUpdates`, `TestTwoConcurrentBetsOf80On100`. Multiprocesso na Fase 10 |
| Unicidade, não negatividade e imutabilidade impostas pelo banco | ✅ | índices únicos, `CHECK (balance_minor >= 0)` e triggers (§4.11) |
| Sem duplicidade, saldo negativo ou perda de evento nas situações listadas | pendente | Fase 10 |

---

## 7. Limitações e trabalho não concluído

- Casos de uso, HTTP, autenticação, consumidor SQS, publisher da outbox, workers e métricas ainda não foram implementados (ver §2).
- As decisões D4, D6 (parte HTTP), D8, D9, D10 e D14 estão pendentes (§4.12).
- A concorrência só foi demonstrada com um processo; a prova com ≥ 3 processos independentes fica para a Fase 10.
- O banco não confere que a transação está `PROCESSED` quando o seu lançamento é gravado (§4.11).
- Moedas limitadas a BRL, USD e EUR, todas com 2 casas decimais.
- LocalStack fixado na linha comunitária `4.14.0`, que não recebe novas correções.
