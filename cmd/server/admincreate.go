package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"mdm/internal/db"
)

// runAdminCreate is the break-glass way in now that the dashboard has no built-in login:
//
//	printf '%s' "$PASSWORD" | server admin-create you@aioapp.com            # new super admin
//	printf '%s' "$PASSWORD" | server admin-create you@aioapp.com --reset    # set a new password
//
// The password is read from stdin (never an argument, so it is not in the process list or
// shell history). It talks to the database only, so it works with the server stopped or
// running: `docker compose exec -T server ./server admin-create ...`. The account is
// verified and has role admin; a --reset keeps its role.
func runAdminCreate(args []string) int {
	var email string
	reset := false
	for _, a := range args {
		switch {
		case a == "--reset":
			reset = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown flag %q\nusage: server admin-create <email> [--reset]   (password on stdin)\n", a)
			return 2
		case email == "":
			email = strings.ToLower(strings.TrimSpace(a))
		default:
			fmt.Fprintln(os.Stderr, "usage: server admin-create <email> [--reset]   (password on stdin)")
			return 2
		}
	}
	if !strings.Contains(email, "@") {
		fmt.Fprintln(os.Stderr, "usage: server admin-create <email> [--reset]   (password on stdin)")
		return 2
	}
	pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 12 {
		fmt.Fprintln(os.Stderr, "the password (read from stdin) must be at least 12 characters")
		return 2
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		getEnv("DB_HOST", "localhost"), getEnv("DB_PORT", "5432"), getEnv("DB_USER", "mdm"), getEnv("DB_PASSWORD", "mdm"), getEnv("DB_NAME", "mdm"))
	database, err := db.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		return 1
	}
	defer database.Close()

	existing, err := database.GetUserByUsername(ctx, email)
	switch {
	case err == nil && existing != nil:
		if !reset {
			fmt.Fprintf(os.Stderr, "%s already has an account (role %s); add --reset to set a new password\n", email, existing.Role)
			return 1
		}
		if err := database.SetUserPassword(ctx, existing.ID, string(hash)); err != nil {
			fmt.Fprintln(os.Stderr, "set password:", err)
			return 1
		}
		fmt.Printf("password reset for %s (role %s)\n", email, existing.Role)
	default:
		if reset {
			fmt.Fprintf(os.Stderr, "no account named %s to reset\n", email)
			return 1
		}
		u, err := database.CreateUser(ctx, email, string(hash), "admin", &email, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			return 1
		}
		_ = database.InsertAudit(ctx, "cli", "user.create", u.Username, "super admin created from the server console (admin-create)")
		fmt.Printf("created super admin %s\n", email)
	}
	return 0
}
