package main

import (
	"bufio"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envReaders are the functions whose first string-literal argument names an
// environment variable.
var envReaders = map[string]bool{"Getenv": true, "LookupEnv": true, "parseDurationEnv": true}

// envVarsReadByCode parses every non-test Go file under the given roots and
// returns the env var names passed as literals to os.Getenv / os.LookupEnv /
// parseDurationEnv.
func envVarsReadByCode(t *testing.T, roots ...string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if !envReaders[name] {
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					found[v] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return found
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestServerEnvVars_MatchCode: ServerEnvVars lists exactly the variables the
// server code reads (cmd/server and pkg/, excluding test helpers).
func TestServerEnvVars_MatchCode(t *testing.T) {
	read := envVarsReadByCode(t, ".", filepath.Join("..", "..", "pkg"))
	delete(read, "TEST_DATABASE_URL") // pgtest helper, test-only
	declared := map[string]bool{}
	for _, v := range ServerEnvVars {
		if declared[v.Name] {
			t.Errorf("ServerEnvVars lists %s twice", v.Name)
		}
		declared[v.Name] = true
	}
	for _, name := range sortedKeys(read) {
		if !declared[name] {
			t.Errorf("code reads %s but ServerEnvVars does not declare it", name)
		}
	}
	for _, name := range sortedKeys(declared) {
		if !read[name] {
			t.Errorf("ServerEnvVars declares %s but the code never reads it", name)
		}
	}
}

// TestEnvExample_DocumentsEveryVariable: .env.example has a line for every
// variable (commented out or not) and only placeholder secret values.
func TestEnvExample_DocumentsEveryVariable(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("open .env.example: %v", err)
	}
	defer func() { _ = f.Close() }()
	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") || strings.ToUpper(k) != k {
			continue
		}
		values[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range ServerEnvVars {
		v, ok := values[ev.Name]
		if !ok {
			t.Errorf(".env.example does not document %s", ev.Name)
			continue
		}
		if ev.Secret && v != "" && !isPlaceholder(v) {
			t.Errorf(".env.example secret %s must be empty or a CHANGE_ME placeholder, got %q", ev.Name, v)
		}
	}
}

// TestValidateProductionConfig_CoversEverySecret: every secret in
// ServerEnvVars has a weak/missing/placeholder value the production guard
// rejects.
func TestValidateProductionConfig_CoversEverySecret(t *testing.T) {
	good := Config{
		DatabaseURL:         "postgres://softpos:Str0ngRandomPassw0rd@db.prod.internal:5432/softpos?sslmode=require",
		RedisAddr:           "redis.prod.internal:6379",
		RedisPassword:       "redis_password_long_enough_123",
		VaultMasterKeyHex:   "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:       "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		JWTSecret:           "prod_jwt_secret_with_at_least_32_random_bytes",
		CoDiWebhookSecret:   "prod_codi_webhook_secret_with_32_random_bytes",
		PlayIntegritySecret: "prod_play_integrity_secret_32_random_bytes",
		VaultPreviousKeys:   "v0:b1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		ReceiptTokenSecret:  "prod_receipt_token_secret_32_random_bytes!",
	}
	if err := ValidateProductionConfig(good, "production"); err != nil {
		t.Fatalf("baseline production config rejected: %v", err)
	}
	bad := map[string][]struct {
		mutate func(*Config)
		want   error
	}{
		"DATABASE_URL": {
			{func(c *Config) { c.DatabaseURL = "" }, ErrMissingDatabaseURL},
			{func(c *Config) { c.DatabaseURL = "postgres://u:p@h/db?sslmode=disable" }, ErrInsecureDatabaseURL},
			{func(c *Config) { c.DatabaseURL = "postgres://softpos:CHANGE_ME@h/db?sslmode=require" }, ErrPlaceholderSecret},
		},
		"REDIS_PASSWORD": {
			{func(c *Config) { c.RedisPassword = "" }, ErrInsecureRedisPassword},
			{func(c *Config) { c.RedisPassword = "CHANGE_ME_redis_password" }, ErrPlaceholderSecret},
		},
		"JWT_SECRET": {
			{func(c *Config) { c.JWTSecret = "short" }, ErrWeakJWTSecret},
			{func(c *Config) { c.JWTSecret = "CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME" }, ErrPlaceholderSecret},
		},
		"PLAY_INTEGRITY_SECRET": {
			{func(c *Config) { c.PlayIntegritySecret = "short" }, ErrWeakPlayIntegritySecret},
		},
		"CODI_WEBHOOK_SECRET": {
			{func(c *Config) { c.CoDiWebhookSecret = "" }, ErrInsecureCoDiWebhookSecret},
		},
		"STP_HMAC_SECRET": {
			{func(c *Config) { c.STPHMACSecret = "" }, ErrInsecureDefaultSTPSecret},
			{func(c *Config) { c.STPHMACSecret = "too_short" }, ErrInsecureDefaultSTPSecret},
			{func(c *Config) { c.STPHMACSecret = defaultSTPHMACSecret }, ErrInsecureDefaultSTPSecret},
		},
		"VAULT_MASTER_KEY_HEX": {
			{func(c *Config) { c.VaultMasterKeyHex = "" }, ErrInsecureDefaultVaultKey},
			{func(c *Config) { c.VaultMasterKeyHex = defaultVaultMasterKeyHex }, ErrInsecureDefaultVaultKey},
		},
		"ACQUIRER_WEBHOOK_SECRET": {
			{func(c *Config) { c.AcquirerProvider, c.AcquirerWebhookSecret = "acme", "" }, ErrInsecureAcquirerSecret},
			{func(c *Config) { c.AcquirerProvider, c.AcquirerWebhookSecret = "acme", "short" }, ErrInsecureAcquirerSecret},
			{func(c *Config) { c.AcquirerWebhookSecret = "CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME" }, ErrPlaceholderSecret},
		},
		"METRICS_TOKEN": {
			{func(c *Config) { c.MetricsAddr, c.MetricsToken = ":9090", "" }, ErrInsecureMetricsToken},
			{func(c *Config) { c.MetricsToken = "short" }, ErrInsecureMetricsToken},
			{func(c *Config) { c.MetricsToken = "CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME" }, ErrPlaceholderSecret},
		},
		"VAULT_PREVIOUS_KEYS": {
			{func(c *Config) { c.VaultPreviousKeys = "v0:nothex" }, ErrInvalidVaultKey},
		},
		"RECEIPT_TOKEN_SECRET": {
			{func(c *Config) { c.ReceiptTokenSecret = "" }, ErrWeakReceiptTokenSecret},
			{func(c *Config) { c.ReceiptTokenSecret = "short" }, ErrWeakReceiptTokenSecret},
			{func(c *Config) { c.ReceiptTokenSecret = "CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME" }, ErrPlaceholderSecret},
		},
	}
	for _, ev := range ServerEnvVars {
		if !ev.Secret {
			continue
		}
		cases, ok := bad[ev.Name]
		if !ok {
			t.Errorf("secret %s has no production-guard case", ev.Name)
			continue
		}
		delete(bad, ev.Name)
		for i, c := range cases {
			cfg := good
			c.mutate(&cfg)
			if err := ValidateProductionConfig(cfg, "production"); !errors.Is(err, c.want) {
				t.Errorf("%s case %d: want %v, got %v", ev.Name, i, c.want, err)
			}
		}
	}
	for name := range bad {
		t.Errorf("guard case for %s, which is not a declared secret", name)
	}
}
