package main

import (
	"fmt"
	"os"
	"time"

	"austro-os/infrastructure/rabbitmq"
	"austro-os/internal/config"
	"austro-os/internal/event"

	"github.com/google/uuid"
)

func main() {
	cfg := config.Load()
	rabbitmq.Initialize(cfg)
	defer rabbitmq.GetConnection().Close()

	env := *event.NewEnvelope()
	env.EventType = event.EventTypeCreated
	env.ActorType = "system"
	env.ActorID = uuid.New()
	env.TargetType = "workspace"
	env.TargetID = uuid.New()
	env.WorkspaceID = "11111111-1111-1111-1111-111111111111"
	env.ConstitutionalPrinciple = "Vision First"
	env.TraceID = uuid.New()
	env.SpanID = uuid.New()

	err := rabbitmq.PublishUniversalEvent(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to publish: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Published event %s (%s)\n", env.EventID, env.EventType)

	// Wait for processing
	time.Sleep(2 * time.Second)
	fmt.Println("Done")
}
