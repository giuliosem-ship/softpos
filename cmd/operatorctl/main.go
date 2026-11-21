// Command operatorctl manages Kerno platform operators (backoffice access)
// directly in the database. There is deliberately no public API for this.
//
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl add +525512345678
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl remove +525512345678
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl check +525512345678
//	DATABASE_URL=postgres://… go run ./cmd/operatorctl load-69b Listado_Completo_69-B.csv [--override-shrink] [--override-changes]
//
// load-69b loads the SAT Article 69-B list from a local file (same parser,
// atomic swap and shrink protection as the server worker, docs/KYB.md) and
// re-checks approved merchants.
//
// The database must already be migrated (cmd/server applies migrations at
// boot). SQL_DRIVER is optional (default pgx). See docs/BACKOFFICE.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: operatorctl add|remove|check <E.164 phone>   (DATABASE_URL required)")
	fmt.Fprintln(os.Stderr, "       operatorctl load-69b <file.csv> [--override-shrink] [--override-changes]")
	os.Exit(2)
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "load-69b" {
		mainLoad69B(os.Args[2:])
		return
	}
	if len(os.Args) != 3 {
		usage()
	}
	cmd, phone := os.Args[1], os.Args[2]
	if !auth.ValidPhone(phone) {
		fmt.Fprintln(os.Stderr, "invalid phone: must be E.164, e.g. +525512345678")
		os.Exit(2)
	}
	db := openDB()
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, store.NewSQLAuthRepository(db), cmd, phone, actor()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func openDB() *sql.DB {
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
	return db
}

func mainLoad69B(args []string) {
	path, opts := "", identity.Load69BOptions{}
	for _, a := range args {
		switch {
		case a == "--override-shrink":
			opts.OverrideShrink = true
		case a == "--override-changes":
			opts.OverrideChanges = true
		case path == "" && !strings.HasPrefix(a, "-"):
			path = a
		default:
			usage()
		}
	}
	if path == "" {
		usage()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read file:", err)
		os.Exit(1)
	}
	db := openDB()
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := runLoad69B(ctx, store.NewSQLBlacklistRepository(db), store.NewSQLKYBRepository(db), store.NewSQLAuditRepository(db),
		raw, "file:"+filepath.Base(path), actor(), opts, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// list69BRepo is the SQL 69-B repository surface load-69b needs.
type list69BRepo interface {
	identity.List69BStore
	identity.BlacklistRepository
}

// runLoad69B loads raw as the SAT 69-B list and re-checks approved merchants.
func runLoad69B(ctx context.Context, repo list69BRepo, kybRepo store.KYBRepository, audit store.OperatorAuditRepository, raw []byte, source, actor string, opts identity.Load69BOptions, out io.Writer) error {
	if opts.OverrideShrink {
		fmt.Fprintln(out, "WARNING: --override-shrink lifts the shrink protection")
	}
	if opts.OverrideChanges {
		fmt.Fprintln(out, "WARNING: --override-changes lifts the situation-change protection")
	}
	syncer := &identity.List69BSyncer{Store: repo, MaxShrinkPct: identity.DefaultMax69BShrinkPct, MaxChangePct: identity.DefaultMax69BChangePct}
	res, err := syncer.LoadWithOptions(ctx, raw, source, actor, opts)
	fmt.Fprintln(out, identity.FormatLoad(res.Load))
	detail := identity.FormatLoad(res.Load) + fmt.Sprintf(" source=%s override_shrink=%t override_changes=%t", source, opts.OverrideShrink, opts.OverrideChanges)
	if err != nil {
		detail += " error=" + err.Error()
	}
	if aerr := audit.AppendAudit(ctx, store.AuditEntry{OperatorID: actor, Action: "sat69b.load_file", Target: res.Load.Version, Detail: detail}); aerr != nil {
		fmt.Fprintln(out, "WARNING: audit write failed:", aerr)
	}
	if err != nil {
		return err
	}
	svc := identity.NewBlacklistService()
	svc.SetRepository(repo)
	retro, err := store.Retroscreen69B(ctx, kybRepo, audit, svc.Lookup, actor)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "retroscreen: %d approved merchants checked, %d blocked %v, %d unblocked %v, %d excepted, %d not conclusive\n",
		retro.Checked, len(retro.Blocked), retro.Blocked, len(retro.Unblocked), retro.Unblocked, len(retro.Excepted), retro.Unknown)
	return nil
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
