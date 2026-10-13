// Package sqs provides the Amazon SQS client and messaging adapters.
package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/health"
)

// Module provides the SQS client and its readiness check.
var Module = fx.Module("sqs",
	fx.Provide(
		NewClient,
		health.AsChecker(NewReadinessCheck),
	),
)

// NewClient builds an SQS client. Credentials come from the standard AWS
// environment variables; the endpoint override points to the local emulator.
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

// NewReadinessCheck verifies that the inbound wager queue is reachable.
func NewReadinessCheck(client *sqs.Client, cfg config.Config) health.CheckerFunc {
	return health.CheckerFunc{
		CheckName: "sqs",
		Fn: func(ctx context.Context) error {
			_, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.WagerQueueName)})
			return err
		},
	}
}
