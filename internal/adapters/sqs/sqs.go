// Package sqs fornece o cliente Amazon SQS e os adaptadores de mensageria.
package sqs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/health"
)

// Module fornece o cliente SQS e sua checagem de readiness.
var Module = fx.Module("sqs",
	fx.Provide(
		NewClient,
		health.AsChecker(NewReadinessCheck),
	),
	fx.Invoke(StartConsumer),
)

// ConsumerName identifica este consumidor na inbox.
const ConsumerName = "wager-transactions-consumer"

// StartConsumer liga o consumidor ao ciclo de vida do Fx. Ele para antes do
// pool do PostgreSQL (ordem inversa de inicialização), então o trabalho em
// andamento termina com o banco ainda disponível.
func StartConsumer(lc fx.Lifecycle, cfg config.Config, client *sqs.Client, svc *app.Service, metrics app.Metrics, log *slog.Logger) {
	if !cfg.ConsumerEnabled {
		log.Info("sqs consumer disabled")
		return
	}
	c := NewConsumer(client, svc, ConsumerConfig{
		Name: ConsumerName, QueueName: cfg.WagerQueueName, DLQName: cfg.WagerDLQName,
		Pollers: cfg.ConsumerPollers, WaitTime: cfg.ConsumerWaitTime, Visibility: cfg.ConsumerVisibility,
		RetryBase: cfg.ConsumerRetryBase, RetryMax: cfg.ConsumerRetryMax,
	}, metrics, log)
	lc.Append(fx.Hook{OnStart: c.Start, OnStop: c.Stop})
}

// NewClient constrói um cliente SQS. As credenciais vêm das variáveis de
// ambiente padrão da AWS; o endpoint sobrescrito aponta para o emulador local.
func NewClient(cfg config.Config) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.AWSEndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.AWSEndpointURL)
		}
	}), nil
}

// NewReadinessCheck verifica se a fila de entrada de wagering está acessível.
func NewReadinessCheck(client *sqs.Client, cfg config.Config) health.CheckerFunc {
	return health.CheckerFunc{
		CheckName: "sqs",
		Fn: func(ctx context.Context) error {
			_, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.WagerQueueName)})
			return err
		},
	}
}
