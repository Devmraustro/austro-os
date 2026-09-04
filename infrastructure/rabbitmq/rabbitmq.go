package rabbitmq

import (
	"fmt"
	"os"

	"austro-os/internal/config"
	"austro-os/internal/event"
	logger "austro-os/internal/log"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	conn      *amqp.Connection
	channel   *amqp.Channel
	queueName = "austro.events"
)

func Initialize(cfg *config.Config) {
	var err error
	conn, err = amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		logger.NewEntry("rabbitmq-connect-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	channel, err = conn.Channel()
	if err != nil {
		logger.NewEntry("rabbitmq-channel-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	if cfg.RabbitMQQueue != "" {
		queueName = cfg.RabbitMQQueue
	}

	_, err = channel.QueueDeclare(
		queueName,
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		logger.NewEntry("rabbitmq-queue-declare-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	logger.NewEntry("rabbitmq-connection-established").With("queue", queueName).Log()
}

func GetChannel() *amqp.Channel {
	return channel
}

func GetConnection() *amqp.Connection {
	return conn
}

func PublishUniversalEvent(env event.UniversalEnvelope) error {
	body, err := env.MarshalJSON()
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	err = channel.Publish(
		"",
		queueName,
		false,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
			MessageId:    env.EventID.String(),
			Headers: amqp.Table{
				"event_type":               string(env.EventType),
				"constitutional_principle": env.ConstitutionalPrinciple,
				"workspace_id":             env.WorkspaceID,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to publish event: %w", err)
	}

	return nil
}
