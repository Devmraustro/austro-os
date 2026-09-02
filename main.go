package main

import (
	"net/http"
	"os"

	"austro-os/infrastructure/database"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/infrastructure/redis"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/config"
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"
)

func main() {
	cfg, err := config.LoadStrict()
	if err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	db := database.Initialize(cfg)
	defer db.Close()

	redisClient := redis.Initialize(cfg)
	defer redisClient.Close()

	rabbitmq.Initialize(cfg)
	defer rabbitmq.GetConnection().Close()

	_ = auth.Initialize(cfg)

	ipBus := event.NewInProcessBus()
	_ = ipBus

	authzService := authz.NewAuthorizer()

	logger.NewEntry("austro-os-startup").With("version", "1.0").With("environment", cfg.Environment).Log()

	mux := http.NewServeMux()

	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ready":true}`))
	})

	// Deny-by-default: every protected route must carry an explicit allow rule.
	// Any request without an explicit permission for its action/resource is DENIED.
	protected := authzMiddleware(authzService, mux)

	logger.NewEntry("austro-os-serving").With("address", cfg.ServerAddress).Log()
	if err := http.ListenAndServe(cfg.ServerAddress, middleware.Middleware(protected)); err != nil {
		logger.NewEntry("http-server-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

// authzMiddleware wraps the mux with the deny-by-default authorization
// enforcement point. Rules are explicit; there is no permissive fallback.
func authzMiddleware(a *authz.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Method
		resource := r.URL.Path

		// Health/liveness probes are public per OpenAPI contracts.
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}

		if err := a.Authorize(r, action, resource); err != nil {
			logger.NewEntry("authorization-denied").
				With("action", action).
				With("resource", resource).
				With("constitutional_principle", "Security by Design").
				Log()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}