#!/bin/bash
# Provisiona as filas SQS FIFO. Política de redrive, visibility timeout e
# destino dos eventos entram quando os parâmetros de mensageria forem definidos.
set -euo pipefail

awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true

awslocal sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes FifoQueue=true
