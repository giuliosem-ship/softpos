// Command operatorctl manages Kerno platform operators (backoffice access)
// directly in the database. There is deliberately no public API for this.
//
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl add +525512345678
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl remove +525512345678
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl check +525512345678
//
// The database must already be migrated (cmd/server applies migrations at
// boot). SQL_DRIVER is optional (default pgx). See docs/BACKOFFICE.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: operatorctl add|remove|check <E.164 phone>   (DATABASE_URL required)")
	os.Exit(2)
}

func main() {
	if len(os.Args) != 3 {
		usage()
	}
	cmd, phone := os.Args[1], os.Args[2]
	if !auth.ValidPhone(phone) {
		fmt.Fprintln(os.Stderr, "invalid phone: must be E.164, e.g. +525512345678")
		os.Exit(2)
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}
	db, err := sql.Open(store.NormalizeDriverName(os.Getenv("SQL_DRIVER")), dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, store.NewSQLAuthRepository(db), cmd, phone, actor()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func actor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "operatorctl:" + u.Username
	}
	return "operatorctl"
}

var errUsage = errors.New("unknown command (want add, remove or check)")

func run(ctx context.Context, repo auth.Repository, cmd, phone, createdBy string) error {
	switch cmd {
	case "add":
		u, found, err := repo.GetUserByPhone(ctx, phone)
		if err != nil {
			return err
		}
		if !found {
			id, err := newUserID()
			if err != nil {
				return err
			}
			u = auth.User{ID: id, Phone: phone, CreatedAt: time.Now().UTC()}
			if err := repo.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := repo.AddPlatformOperator(ctx, u.ID, createdBy); err != nil {
			return err
		}
		fmt.Printf("platform operator %s (%s) added\n", phone, u.ID)
	case "remove", "check":
		u, found, err := repo.GetUserByPhone(ctx, phone)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no user with phone %s", phone)
		}
		if cmd == "remove" {
			if err := repo.RemovePlatformOperator(ctx, u.ID); err != nil {
				return err
			}
			fmt.Printf("platform operator %s (%s) removed\n", phone, u.ID)
			return nil
		}
		ok, err := repo.IsPlatformOperator(ctx, u.ID)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) platform operator: %t\n", phone, u.ID, ok)
	default:
		return errUsage
	}
	return nil
}
