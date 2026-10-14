// Package sns publica os eventos da outbox no tópico SNS FIFO (D10).
package sns

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sns/types"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/platform/config"
)

// NewClient constrói um cliente SNS com as credenciais padrão da AWS e o
// endpoint do emulador local, quando configurado.
func NewClient(cfg config.Config) (*sns.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return sns.NewFromConfig(awsCfg, func(o *sns.Options) {
		if cfg.AWSEndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.AWSEndpointURL)
		}
	}), nil
}

// Publisher implementa app.EventPublisher sobre um tópico SNS FIFO.
type Publisher struct {
	client   *sns.Client
	topicARN string
}

// NewPublisher cria o publisher para o tópico.
func NewPublisher(client *sns.Client, topicARN string) *Publisher {
	return &Publisher{client: client, topicARN: topicARN}
}

// Publish envia o snapshot do evento sem alteração:
//   - MessageGroupId = aggregateId: ordem por agregado no broker;
//   - MessageDeduplicationId = eventId: republicações dentro da janela de 5
//     minutos são descartadas pelo SNS; depois dela, o consumidor deduplica
//     pelo eventId;
//   - atributos eventType e eventVersion, para filtro por tipo.
func (p *Publisher) Publish(ctx context.Context, e app.OutboxEvent) error {
	_, err := p.client.Publish(ctx, &sns.PublishInput{
		TopicArn:               aws.String(p.topicARN),
		Message:                aws.String(string(e.Payload)),
		MessageGroupId:         aws.String(e.AggregateID),
		MessageDeduplicationId: aws.String(e.ID.String()),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType":    {DataType: aws.String("String"), StringValue: aws.String(e.EventType)},
			"eventVersion": {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(e.EventVersion))},
		},
	})
	return err
}
