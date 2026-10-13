#!/bin/bash
# Provisions the SQS FIFO queues. Redrive policy, visibility timeout and
# event destination are added once the messaging parameters are defined.
set -euo pipefail

awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true

awslocal sqs create-queue \
  --queue-name wager-transactions.fifo \
  --attributes FifoQueue=true
