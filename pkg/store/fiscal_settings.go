package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Global invoice periodicities (merchant_fiscal_settings.global_periodicity).
const (
	GlobalDaily   = "DAILY"
	GlobalMonthly = "MONTHLY"

	AutofacturaSameMonth = "SAME_MONTH"
	AutofacturaDisabled  = "DISABLED"
)

// FiscalSettings are a merchant's CFDI defaults (migration 016).
type FiscalSettings struct {
	MerchantID        string
	ClaveProdServ     string
	ClaveUnidad       string
	Descripcion       string
	LugarExpedicion   string // "" = postal code of the KYB address
	GlobalPeriodicity string
	// Autofactura (MVP-26): AutofacturaSameMonth (default) lets the end
	// customer invoice a sale from its receipt during the sale's calendar
	// month (America/Mexico_City); AutofacturaDisabled turns it off.
	Autofactura string
	UpdatedBy   string
	UpdatedAt         time.Time
}

// DefaultFiscalSettings are used until the owner saves settings.
func DefaultFiscalSettings(merchantID string) FiscalSettings {
	return FiscalSettings{MerchantID: merchantID, ClaveProdServ: "01010101", ClaveUnidad: "H87", Descripcion: "Venta", GlobalPeriodicity: GlobalMonthly,
		Autofactura: AutofacturaSameMonth}
}

// FiscalSettingsRepository persists FiscalSettings.
type FiscalSettingsRepository interface {
	GetFiscalSettings(ctx context.Context, merchantID string) (FiscalSettings, bool, error)
	SaveFiscalSettings(ctx context.Context, fs FiscalSettings) error
}

var (
	_ FiscalSettingsRepository = (*SQLFiscalSettingsRepository)(nil)
	_ FiscalSettingsRepository = (*MemoryFiscalSettingsRepository)(nil)
)

// SQLFiscalSettingsRepository implements FiscalSettingsRepository on PostgreSQL.
type SQLFiscalSettingsRepository struct{ db *sql.DB }

func NewSQLFiscalSettingsRepository(db *sql.DB) *SQLFiscalSettingsRepository {
	return &SQLFiscalSettingsRepository{db: db}
}

func (r *SQLFiscalSettingsRepository) GetFiscalSettings(ctx context.Context, merchantID string) (FiscalSettings, bool, error) {
	fs := FiscalSettings{MerchantID: merchantID}
	err := r.db.QueryRowContext(ctx, `SELECT clave_prod_serv, clave_unidad, descripcion, lugar_expedicion, global_periodicity, autofactura,
		updated_by, updated_at FROM merchant_fiscal_settings WHERE merchant_id = $1`, merchantID).Scan(&fs.ClaveProdServ, &fs.ClaveUnidad,
		&fs.Descripcion, &fs.LugarExpedicion, &fs.GlobalPeriodicity, &fs.Autofactura, &fs.UpdatedBy, &fs.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultFiscalSettings(merchantID), false, nil
	}
	if err != nil {
		return FiscalSettings{}, false, fmt.Errorf("get fiscal settings: %w", err)
	}
	fs.UpdatedAt = fs.UpdatedAt.UTC()
	return fs, true, nil
}

func (r *SQLFiscalSettingsRepository) SaveFiscalSettings(ctx context.Context, fs FiscalSettings) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO merchant_fiscal_settings
		(merchant_id, clave_prod_serv, clave_unidad, descripcion, lugar_expedicion, global_periodicity, updated_by, updated_at, autofactura)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (merchant_id) DO UPDATE SET clave_prod_serv = EXCLUDED.clave_prod_serv, clave_unidad = EXCLUDED.clave_unidad,
			descripcion = EXCLUDED.descripcion, lugar_expedicion = EXCLUDED.lugar_expedicion,
			global_periodicity = EXCLUDED.global_periodicity, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at,
			autofactura = EXCLUDED.autofactura`,
		fs.MerchantID, fs.ClaveProdServ, fs.ClaveUnidad, fs.Descripcion, fs.LugarExpedicion, fs.GlobalPeriodicity, fs.UpdatedBy, time.Now().UTC(),
		autofacturaOrDefault(fs.Autofactura))
	if err != nil {
		return fmt.Errorf("save fiscal settings: %w", err)
	}
	return nil
}

// MemoryFiscalSettingsRepository is the in-memory implementation.
type MemoryFiscalSettingsRepository struct {
	mu sync.RWMutex
	m  map[string]FiscalSettings
}

func NewMemoryFiscalSettingsRepository() *MemoryFiscalSettingsRepository {
	return &MemoryFiscalSettingsRepository{m: map[string]FiscalSettings{}}
}

func (r *MemoryFiscalSettingsRepository) GetFiscalSettings(_ context.Context, merchantID string) (FiscalSettings, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fs, ok := r.m[merchantID]
	if !ok {
		return DefaultFiscalSettings(merchantID), false, nil
	}
	return fs, true, nil
}

func (r *MemoryFiscalSettingsRepository) SaveFiscalSettings(_ context.Context, fs FiscalSettings) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fs.UpdatedAt = time.Now().UTC()
	fs.Autofactura = autofacturaOrDefault(fs.Autofactura)
	r.m[fs.MerchantID] = fs
	return nil
}

func autofacturaOrDefault(v string) string {
	if v == "" {
		return AutofacturaSameMonth
	}
	return v
}
