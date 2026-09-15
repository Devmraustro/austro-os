// Command browser-e2e-setup provisions and removes isolated identities for the
// real browser verification job. It is test infrastructure only: it does not
// change the application authentication or authorization path.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/auth"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	mode := flag.String("mode", "setup", "setup or cleanup")
	flag.Parse()

	dsn := requiredEnv("AUSTRO_POSTGRES_DSN")
	envFile := requiredEnv("BROWSER_E2E_ENV_FILE")

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fatal("open database", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fatal("ping database", err)
	}

	switch *mode {
	case "setup":
		setup(ctx, db, envFile)
	case "cleanup":
		cleanup(ctx, db, envFile)
	default:
		fatalf("unsupported mode %q", *mode)
	}
}

func setup(ctx context.Context, db *sql.DB, envFile string) {
	password := requiredEnv("BROWSER_E2E_PASSWORD")
	if len(password) < 16 {
		fatalf("BROWSER_E2E_PASSWORD must be at least 16 characters")
	}

	suffix := strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", ""))
	workspaceAName := "browser-e2e-a-" + suffix[:12]
	workspaceBName := "browser-e2e-b-" + suffix[:12]
	workspaceStore := postgres.NewWorkspaceStore(db)
	workspaceA, err := workspaceStore.Create(ctx, workspaceAName)
	if err != nil {
		fatal("create workspace A", err)
	}
	workspaceB, err := workspaceStore.Create(ctx, workspaceBName)
	if err != nil {
		fatal("create workspace B", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		fatal("hash browser password", err)
	}
	userStore := postgres.NewUserStore(db)
	users := []struct {
		username string
		name     string
		role     rbac.Role
		ws       string
	}{
		{"browser-e2e-admin-a-" + suffix[:12], "Browser E2E Admin A", rbac.RoleWorkspaceAdmin, workspaceA.ID.String()},
		{"browser-e2e-member-a-" + suffix[:12], "Browser E2E Member A", rbac.RoleWorkspaceMember, workspaceA.ID.String()},
		{"browser-e2e-admin-b-" + suffix[:12], "Browser E2E Admin B", rbac.RoleWorkspaceAdmin, workspaceB.ID.String()},
	}
	for _, candidate := range users {
		_, err := userStore.Create(ctx, &auth.UserRecord{
			Username:     candidate.username,
			DisplayName:  candidate.name,
			Role:         candidate.role,
			WorkspaceID:  candidate.ws,
			PasswordHash: hash,
		})
		if err != nil {
			fatal("create browser identity", err)
		}
	}

	writeEnv(envFile, map[string]string{
		"BROWSER_E2E_WORKSPACE_A":      workspaceA.ID.String(),
		"BROWSER_E2E_WORKSPACE_B":      workspaceB.ID.String(),
		"BROWSER_E2E_WORKSPACE_A_NAME": workspaceA.Name,
		"BROWSER_E2E_WORKSPACE_B_NAME": workspaceB.Name,
		"BROWSER_E2E_ADMIN_A":           users[0].username,
		"BROWSER_E2E_MEMBER_A":          users[1].username,
		"BROWSER_E2E_ADMIN_B":           users[2].username,
	})
	fmt.Println("browser E2E data prepared")
}

func cleanup(ctx context.Context, db *sql.DB, envFile string) {
	values := readEnv(envFile)
	for _, id := range []string{values["BROWSER_E2E_WORKSPACE_A"], values["BROWSER_E2E_WORKSPACE_B"]} {
		if id == "" {
			fatalf("browser E2E environment is missing a workspace id")
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM audit_events WHERE workspace_id = $1`, id); err != nil {
			fatal("delete browser audit rows", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE workspace_id = $1`, id); err != nil {
			fatal("delete browser identities", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, id); err != nil {
			fatal("delete browser workspace", err)
		}
	}
	fmt.Println("browser E2E data cleaned")
}

func writeEnv(path string, values map[string]string) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		fatal("open browser E2E environment file", err)
	}
	defer file.Close()
	for _, key := range []string{
		"BROWSER_E2E_WORKSPACE_A", "BROWSER_E2E_WORKSPACE_B",
		"BROWSER_E2E_WORKSPACE_A_NAME", "BROWSER_E2E_WORKSPACE_B_NAME",
		"BROWSER_E2E_ADMIN_A", "BROWSER_E2E_MEMBER_A", "BROWSER_E2E_ADMIN_B",
	} {
		value := values[key]
		if value == "" || strings.ContainsAny(value, "\r\n'\"") {
			fatalf("unsafe browser E2E environment value for %s", key)
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			fatal("write browser E2E environment file", err)
		}
	}
}

func readEnv(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read browser E2E environment file", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		fatalf("%s is required", name)
	}
	return value
}

func fatal(action string, err error) {
	fatalf("%s: %v", action, err)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "browser E2E setup failed: "+format+"\n", args...)
	os.Exit(1)
}
