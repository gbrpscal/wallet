// Package config carrega e valida a configuração do processo a partir das
// variáveis de ambiente.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Config reúne todas as configurações de que o processo precisa. As
// credenciais da AWS não fazem parte dela: o SDK da AWS as lê direto do
// ambiente, para que nunca passem por structs da aplicação nem por logs.
type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	AWSRegion         string
	AWSEndpointURL    string
	WagerQueueName    string
	LogLevel          slog.Level
	ShutdownTimeout   time.Duration
	ReadinessTimeout  time.Duration
	HTTPReadTimeout   time.Duration
	HTTPWriteTimeout  time.Duration
	HTTPHeaderTimeout time.Duration
}

// Load lê o ambiente e devolve um Config validado.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		HTTPAddr:       stringOr("HTTP_ADDR", ":8080"),
		DatabaseURL:    stringOr("DATABASE_URL", ""),
		AWSRegion:      stringOr("AWS_REGION", "us-east-1"),
		AWSEndpointURL: stringOr("AWS_ENDPOINT_URL", ""),
		WagerQueueName: stringOr("SQS_WAGER_QUEUE_NAME", "wager-transactions.fifo"),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(stringOr("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	cfg.ShutdownTimeout = durationOr("SHUTDOWN_TIMEOUT", 30*time.Second, &errs)
	cfg.ReadinessTimeout = durationOr("READINESS_TIMEOUT", 2*time.Second, &errs)
	cfg.HTTPReadTimeout = durationOr("HTTP_READ_TIMEOUT", 10*time.Second, &errs)
	cfg.HTTPWriteTimeout = durationOr("HTTP_WRITE_TIMEOUT", 15*time.Second, &errs)
	cfg.HTTPHeaderTimeout = durationOr("HTTP_READ_HEADER_TIMEOUT", 5*time.Second, &errs)

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func stringOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration, errs *[]error) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return 0
	}
	if d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s must be positive", key))
		return 0
	}
	return d
}
