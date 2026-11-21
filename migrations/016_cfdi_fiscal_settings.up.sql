-- 016_cfdi_fiscal_settings.up.sql (MVP-21)
-- Per-merchant CFDI defaults: concept keys (c_ClaveProdServ / c_ClaveUnidad
-- subset of the embedded SAT catalogs), concept description, lugar de
-- expedición (CP) override and the factura global periodicity. The CSD
-- itself stays in merchants.encrypted_csd_json (vault envelope).

CREATE TABLE IF NOT EXISTS merchant_fiscal_settings (
    merchant_id VARCHAR(128) PRIMARY KEY REFERENCES merchants(id) ON DELETE CASCADE,
    clave_prod_serv VARCHAR(8) NOT NULL DEFAULT '01010101',
    clave_unidad VARCHAR(3) NOT NULL DEFAULT 'H87',
    descripcion VARCHAR(1000) NOT NULL DEFAULT 'Venta',
    lugar_expedicion VARCHAR(5) NOT NULL DEFAULT '',
    global_periodicity VARCHAR(8) NOT NULL DEFAULT 'MONTHLY' CHECK (global_periodicity IN ('DAILY', 'MONTHLY')),
    updated_by VARCHAR(64) NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
