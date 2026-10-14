#!/bin/bash
# Provisiona as filas SQS FIFO de entrada (D9):
#   wager-transactions-dlq.fifo  dead-letter queue (retenção de 14 dias)
#   wager-transactions.fifo      visibility timeout de 30s; após 5 recebimentos
#                                sem remoção, o redrive move a mensagem para a DLQ
# e as políticas de acesso: só o produtor envia; só o consumidor recebe, remove
# e altera visibilidade; o consumidor também envia para a DLQ (mensagens
# inválidas). O LocalStack comunitário não aplica IAM: as políticas ficam
# registradas nas filas e valem tal qual na AWS.
set -euo pipefail

ACCOUNT=000000000000
REGION=us-east-1
PRODUCER="arn:aws:iam::${ACCOUNT}:user/wager-producer"
CONSUMER="arn:aws:iam::${ACCOUNT}:user/wallet-consumer"
MAIN_ARN="arn:aws:sqs:${REGION}:${ACCOUNT}:wager-transactions.fifo"
DLQ_ARN="arn:aws:sqs:${REGION}:${ACCOUNT}:wager-transactions-dlq.fifo"

for user in wager-producer wallet-consumer; do
  awslocal iam create-user --user-name "$user" >/dev/null 2>&1 || true
done

main_policy=$(cat <<JSON
{"Version":"2012-10-17","Statement":[
 {"Sid":"ProducerSends","Effect":"Allow","Principal":{"AWS":"${PRODUCER}"},"Action":"sqs:SendMessage","Resource":"${MAIN_ARN}"},
 {"Sid":"ConsumerReads","Effect":"Allow","Principal":{"AWS":"${CONSUMER}"},
  "Action":["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:ChangeMessageVisibility","sqs:GetQueueAttributes","sqs:GetQueueUrl"],
  "Resource":"${MAIN_ARN}"}]}
JSON
)
dlq_policy=$(cat <<JSON
{"Version":"2012-10-17","Statement":[
 {"Sid":"ConsumerDeadLetters","Effect":"Allow","Principal":{"AWS":"${CONSUMER}"},
  "Action":["sqs:SendMessage","sqs:GetQueueUrl"],"Resource":"${DLQ_ARN}"},
 {"Sid":"RedriveFromMain","Effect":"Allow","Principal":{"Service":"sqs.amazonaws.com"},
  "Action":"sqs:SendMessage","Resource":"${DLQ_ARN}","Condition":{"ArnEquals":{"aws:SourceArn":"${MAIN_ARN}"}}}]}
JSON
)

attrs() {
  # Converte pares chave=valor em JSON de atributos, escapando os valores.
  python3 -c 'import json,sys; print(json.dumps(dict(a.split("=",1) for a in sys.argv[1:])))' "$@"
}

awslocal sqs create-queue --queue-name wager-transactions-dlq.fifo \
  --attributes "$(attrs FifoQueue=true MessageRetentionPeriod=1209600 "Policy=${dlq_policy}")" >/dev/null

awslocal sqs create-queue --queue-name wager-transactions.fifo \
  --attributes "$(attrs FifoQueue=true ContentBasedDeduplication=false VisibilityTimeout=30 \
    "RedrivePolicy={\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"5\"}" \
    "Policy=${main_policy}")" >/dev/null

echo "wager queues ready"
