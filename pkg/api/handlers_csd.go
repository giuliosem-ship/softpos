package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"softpos/pkg/auth"
	"softpos/pkg/fiscal"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/store"
)

// CFDI correctness (MVP-21): merchant CSD upload, SAT catalogs and merchant
// fiscal defaults. See docs/CFDI.md.

// SetFiscalSettingsRepository replaces the in-memory CFDI settings store.
func (s *Server) SetFiscalSettingsRepository(repo store.FiscalSettingsRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fiscalSettings = repo
}

func (s *Server) fiscalSettingsRepo() store.FiscalSettingsRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fiscalSettings
}

// csdSigner builds a signer from a vault-opened CSD (plaintext demo keys or
// SAT encrypted .key + password).
func csdSigner(csd identity.DecryptedCSD) (*fiscal.CSDSigner, error) {
	if csd.PlaintextKey() {
		return fiscal.NewCSDSigner(csd.CERBytes, csd.KeyBytes)
	}
	return fiscal.NewCSDSignerFromSAT(csd.CERBytes, csd.KeyBytes, csd.Password)
}

func (s *Server) csdSigner(csd identity.DecryptedCSD) (*fiscal.CSDSigner, error) { return csdSigner(csd) }

// merchantSigner opens the merchant's CSD and returns a signer; the
// decrypted material is wiped before returning.
func (s *Server) merchantSigner(merchantID string) (*fiscal.CSDSigner, error) {
	csd, err := s.getMerchantCSD(merchantID)
	if err != nil {
		return nil, err
	}
	defer csd.Wipe()
	return csdSigner(csd)
}

// plainCSDKey returns the plaintext PKCS#8 key of an opened CSD (caller clears it).
func plainCSDKey(csd identity.DecryptedCSD) ([]byte, error) {
	if csd.PlaintextKey() {
		return append([]byte(nil), csd.KeyBytes...), nil
	}
	return fiscal.DecryptSATPrivateKey(csd.KeyBytes, csd.Password)
}

// satRegimeCode maps the KYB tax regime to c_RegimenFiscal.
func satRegimeCode(r kyb.TaxRegime) string {
	switch r {
	case kyb.RegimeRESICOPF, kyb.RegimeRESICOPM:
		return "626"
	case kyb.RegimeActividadEmpresarialPF:
		return "612"
	case kyb.RegimePlataformasTecnologicas:
		return "625"
	case kyb.RegimeGeneralPM:
		return "601"
	}
	return ""
}

// emisorProfile is the issuer data of a merchant's CFDI.
type emisorProfile struct {
	RFC             string
	Nombre          string
	Regimen         string
	LugarExpedicion string
	Settings        store.FiscalSettings
}

// errFiscalProfile lists the issuer fields that are missing.
type errFiscalProfile struct{ missing []string }

func (e errFiscalProfile) Error() string {
	return "fiscal profile incomplete: " + strings.Join(e.missing, ",")
}

// merchantRFC is the verified RFC (KYB application) or the profile RFC.
func (s *Server) merchantRFC(ctx context.Context, merchantID string) (string, kyb.Application, error) {
	repo, _ := s.kybStore()
	var app kyb.Application
	if rec, found, err := repo.GetKYB(ctx, merchantID); err != nil {
		return "", app, err
	} else if found {
		app = rec.Application.Normalize()
	}
	rfc := app.RFC
	if rfc == "" {
		if m, ok := s.GetMerchant(merchantID); ok {
			rfc = strings.ToUpper(strings.TrimSpace(m.RFC))
		}
	}
	return rfc, app, nil
}

func (s *Server) emisorProfile(ctx context.Context, merchantID string) (emisorProfile, error) {
	rfc, app, err := s.merchantRFC(ctx, merchantID)
	if err != nil {
		return emisorProfile{}, err
	}
	settings, _, err := s.fiscalSettingsRepo().GetFiscalSettings(ctx, merchantID)
	if err != nil {
		return emisorProfile{}, err
	}
	p := emisorProfile{RFC: rfc, Nombre: strings.ToUpper(strings.TrimSpace(app.LegalName)), Regimen: satRegimeCode(app.TaxRegime),
		LugarExpedicion: app.Address.PostalCode, Settings: settings}
	if p.Nombre == "" {
		if m, ok := s.GetMerchant(merchantID); ok {
			p.Nombre = strings.ToUpper(strings.TrimSpace(m.Name))
		}
	}
	if settings.LugarExpedicion != "" {
		p.LugarExpedicion = settings.LugarExpedicion
	}
	var missing []string
	if p.RFC == "" {
		missing = append(missing, "rfc")
	}
	if p.Nombre == "" {
		missing = append(missing, "legal_name")
	}
	if p.Regimen == "" {
		missing = append(missing, "tax_regime")
	}
	if !fiscal.ValidPostalCode(p.LugarExpedicion) {
		missing = append(missing, "lugar_expedicion")
	}
	if len(missing) > 0 {
		return p, errFiscalProfile{missing}
	}
	return p, nil
}

// CSDResponse is the CSD metadata (never key material).
type CSDResponse struct {
	Serial    string `json:"serial"`
	RFC       string `json:"rfc"`
	ValidFrom string `json:"valid_from"`
	ValidTo   string `json:"valid_to"`
	Status    string `json:"status"` // ACTIVE | EXPIRED
}

func csdResponse(info fiscal.CSDInfo, now time.Time) CSDResponse {
	st := "ACTIVE"
	if now.After(info.ValidTo) {
		st = "EXPIRED"
	}
	return CSDResponse{Serial: info.Serial, RFC: info.RFC, ValidFrom: info.ValidFrom.Format(time.RFC3339),
		ValidTo: info.ValidTo.Format(time.RFC3339), Status: st}
}

// maxCSDPart bounds each uploaded part (real .cer/.key are ~1-2 KB).
const maxCSDPart = 64 << 10

func writeCSDInvalid(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "CSD_INVALID", "reason": reason})
}

// readCSDUpload reads the multipart parts cer, key and password as bytes
// (the password never becomes a Go string).
func readCSDUpload(r *http.Request) (cer, key, password []byte, err error) {
	ct, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if perr != nil || ct != "multipart/form-data" || params["boundary"] == "" {
		return nil, nil, nil, errors.New("not multipart")
	}
	mr, merr := r.MultipartReader()
	if merr != nil {
		return nil, nil, nil, merr
	}
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return cer, key, password, perr
		}
		var dst *[]byte
		switch part.FormName() {
		case "cer":
			dst = &cer
		case "key":
			dst = &key
		case "password":
			dst = &password
		default:
			_ = part.Close()
			continue
		}
		var buf bytes.Buffer
		n, cerr := io.Copy(&buf, io.LimitReader(part, maxCSDPart+1))
		_ = part.Close()
		if cerr != nil || n > maxCSDPart {
			clear(buf.Bytes())
			return cer, key, password, errors.New("part too large")
		}
		*dst = buf.Bytes()
	}
	if password != nil {
		password = bytes.TrimRight(password, "\r\n")
	}
	return cer, key, password, nil
}

// HandlePutCSD: PUT /api/v1/merchants/{id}/csd (owner; multipart cer, key,
// password). Validates and stores the CSD encrypted in the vault.
func (s *Server) HandlePutCSD(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*maxCSDPart)
	cer, key, password, err := readCSDUpload(r)
	defer func() { clear(key); clear(password) }()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MULTIPART")
		return
	}
	switch {
	case len(cer) == 0:
		writeCSDInvalid(w, "MISSING_CER")
		return
	case len(key) == 0:
		writeCSDInvalid(w, "MISSING_KEY")
		return
	case len(password) == 0:
		writeCSDInvalid(w, "MISSING_PASSWORD")
		return
	}
	ctx := r.Context()
	rfc, _, err := s.merchantRFC(ctx, tenant)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "KYB_UNAVAILABLE")
		return
	}
	if rfc == "" {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	now := nowUTC()
	info, certDER, err := fiscal.ValidateCSD(cer, key, password, rfc, now)
	if err != nil {
		reason := fiscal.CSDReason(err)
		if reason == "" {
			reason = fiscal.CSDReasonKeyUnreadable
		}
		slog.Warn("CSD upload rejected", "merchant_id", tenant, "reason", reason)
		writeCSDInvalid(w, reason)
		return
	}
	enc, err := s.vault().StoreCSDBytes(tenant, certDER, key, password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CSD_VAULT_FAILED")
		return
	}
	encBytes, err := json.Marshal(enc)
	if err == nil {
		err = s.merchantRepo.SaveMerchantCSD(ctx, tenant, encBytes)
	}
	if err != nil {
		slog.Error("CSD save failed", "merchant_id", tenant, "error", err)
		writeError(w, http.StatusServiceUnavailable, "CSD_STORE_FAILED")
		return
	}
	user, _ := UserIDFromContext(ctx)
	slog.Info("merchant CSD stored", "merchant_id", tenant, "serial", info.Serial, "valid_to", info.ValidTo, "user", user)
	writeJSON(w, http.StatusOK, csdResponse(info, now))
}

// storedCSDInfo reads the metadata of the stored CSD from its certificate.
func (s *Server) storedCSDInfo(merchantID string) (fiscal.CSDInfo, bool, error) {
	if _, found, err := s.merchantRepo.GetMerchantCSD(context.Background(), merchantID); err != nil || !found {
		return fiscal.CSDInfo{}, false, err
	}
	signer, err := s.merchantSigner(merchantID)
	if err != nil {
		return fiscal.CSDInfo{}, true, err
	}
	cert := signer.Certificate()
	return fiscal.CSDInfo{Serial: fiscal.CertificateNumber(cert), RFC: fiscal.CertificateRFC(cert), Subject: cert.Subject.CommonName,
		ValidFrom: cert.NotBefore.UTC(), ValidTo: cert.NotAfter.UTC()}, true, nil
}

// HandleGetCSD: GET /api/v1/merchants/{id}/csd (any member).
func (s *Server) HandleGetCSD(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	info, found, err := s.storedCSDInfo(tenant)
	switch {
	case err != nil && found:
		slog.Error("stored CSD unreadable", "merchant_id", tenant, "error", err)
		writeError(w, http.StatusInternalServerError, "CSD_UNREADABLE")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "CSD_STORE_FAILED")
	case !found:
		writeError(w, http.StatusNotFound, "CSD_NOT_FOUND")
	default:
		writeJSON(w, http.StatusOK, csdResponse(info, nowUTC()))
	}
}

// HandleSATCatalogs: GET /api/v1/catalogs/sat?names=regimen_fiscal,uso_cfdi
// (any authenticated user; no names = every catalog).
func (s *Server) HandleSATCatalogs(w http.ResponseWriter, r *http.Request) {
	names := fiscal.CatalogNames
	if v := strings.TrimSpace(r.URL.Query().Get("names")); v != "" {
		names = strings.Split(v, ",")
	}
	out := map[string]any{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		c, ok := fiscal.CatalogByName(n)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "UNKNOWN_CATALOG", "name": n})
			return
		}
		out[n] = c
	}
	w.Header().Set("X-SAT-Catalog-Version", fiscal.Catalogs().Version)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	writeJSON(w, http.StatusOK, out)
}

// FiscalSettingsBody is GET/PUT /api/v1/merchants/{id}/fiscal-settings.
type FiscalSettingsBody struct {
	ClaveProdServ     string `json:"clave_prod_serv"`
	ClaveUnidad       string `json:"clave_unidad"`
	Descripcion       string `json:"descripcion"`
	LugarExpedicion   string `json:"lugar_expedicion"`
	GlobalPeriodicity string `json:"global_periodicity"` // DAILY | MONTHLY
}

func fiscalSettingsBody(fs store.FiscalSettings, lugar string) FiscalSettingsBody {
	if fs.LugarExpedicion != "" {
		lugar = fs.LugarExpedicion
	}
	return FiscalSettingsBody{ClaveProdServ: fs.ClaveProdServ, ClaveUnidad: fs.ClaveUnidad, Descripcion: fs.Descripcion,
		LugarExpedicion: lugar, GlobalPeriodicity: fs.GlobalPeriodicity}
}

// HandleGetFiscalSettings: GET (any member); lugar_expedicion falls back to the KYB postal code.
func (s *Server) HandleGetFiscalSettings(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	p, err := s.emisorProfile(r.Context(), tenant)
	var incomplete errFiscalProfile
	if err != nil && !errors.As(err, &incomplete) {
		writeError(w, http.StatusServiceUnavailable, "FISCAL_SETTINGS_UNAVAILABLE")
		return
	}
	writeJSON(w, http.StatusOK, fiscalSettingsBody(p.Settings, p.LugarExpedicion))
}

// HandlePutFiscalSettings: PUT (owner). Empty fields keep their defaults.
func (s *Server) HandlePutFiscalSettings(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner) {
		return
	}
	var body FiscalSettingsBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	ctx := r.Context()
	fs, _, err := s.fiscalSettingsRepo().GetFiscalSettings(ctx, tenant)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "FISCAL_SETTINGS_UNAVAILABLE")
		return
	}
	bad := map[string]string{}
	if v := strings.TrimSpace(body.ClaveProdServ); v != "" {
		if !fiscal.ValidClaveProdServ(v) {
			bad["clave_prod_serv"] = fiscal.ReasonUnknownCode
		}
		fs.ClaveProdServ = v
	}
	if v := strings.ToUpper(strings.TrimSpace(body.ClaveUnidad)); v != "" {
		if !fiscal.ValidClaveUnidad(v) {
			bad["clave_unidad"] = fiscal.ReasonUnknownCode
		}
		fs.ClaveUnidad = v
	}
	if v := strings.TrimSpace(body.Descripcion); v != "" {
		if utf8.RuneCountInString(v) > 1000 || !fiscal.ValidXMLText(v) {
			bad["descripcion"] = fiscal.ReasonInvalidFormat
		}
		fs.Descripcion = v
	}
	if v := strings.TrimSpace(body.LugarExpedicion); v != "" {
		if !fiscal.ValidPostalCode(v) {
			bad["lugar_expedicion"] = fiscal.ReasonInvalidFormat
		}
		fs.LugarExpedicion = v
	}
	if v := strings.ToUpper(strings.TrimSpace(body.GlobalPeriodicity)); v != "" {
		if v != store.GlobalDaily && v != store.GlobalMonthly {
			bad["global_periodicity"] = fiscal.ReasonUnknownCode
		}
		fs.GlobalPeriodicity = v
	}
	if len(bad) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "FISCAL_SETTINGS_INVALID", "fields": bad})
		return
	}
	fs.MerchantID = tenant
	fs.UpdatedBy, _ = UserIDFromContext(ctx)
	if err := s.fiscalSettingsRepo().SaveFiscalSettings(ctx, fs); err != nil {
		writeError(w, http.StatusServiceUnavailable, "FISCAL_SETTINGS_UNAVAILABLE")
		return
	}
	p, _ := s.emisorProfile(ctx, tenant)
	writeJSON(w, http.StatusOK, fiscalSettingsBody(fs, p.LugarExpedicion))
}

// SeedFiscalSettings stores CFDI defaults from trusted server code (DEV_MODE
// demo merchant without a KYB address).
func (s *Server) SeedFiscalSettings(ctx context.Context, merchantID, lugarExpedicion string) error {
	fs, found, err := s.fiscalSettingsRepo().GetFiscalSettings(ctx, merchantID)
	if err != nil || found {
		return err
	}
	if !fiscal.ValidPostalCode(lugarExpedicion) {
		return fmt.Errorf("invalid lugar de expedicion %q", lugarExpedicion)
	}
	fs.LugarExpedicion, fs.UpdatedBy = lugarExpedicion, "system:seed"
	return s.fiscalSettingsRepo().SaveFiscalSettings(ctx, fs)
}
