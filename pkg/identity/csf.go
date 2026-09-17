package identity

import (
	"errors"
	"regexp"
)

var (
	ErrInvalidCSF = errors.New("invalid or missing required fields in CSF document")

	rfcRegex          = regexp.MustCompile(`RFC:\s*([A-Z0-9]{12,13})`)
	codigoPostalRegex = regexp.MustCompile(`Código Postal:\s*(\d{5})`)
	razonSocialRegex  = regexp.MustCompile(`Denominación/Razón Social:\s*(.+?)(?:\n|Régimen|Datos|$)`)
	estatusRegex      = regexp.MustCompile(`Estatus en el padrón:\s*([A-Z]+)`)
	regimenRegex      = regexp.MustCompile(`Régimen:\s*(\d{3})\s*-\s*([^\n\r]+?)(?:\s+Fecha|\n|$)`)
)

type RegimenFiscal struct {
	Clave       string
	Descripcion string
}

type CSFProfile struct {
	RFC          string
	RazonSocial  string
	CodigoPostal string
	Regimenes    []RegimenFiscal
	EstatusPadron string
	IsResico     bool
}

func ParseCSFText(text string) (CSFProfile, error) {
	var profile CSFProfile

	// Extract RFC
	matches := rfcRegex.FindStringSubmatch(text)
	if len(matches) < 2 {
		return CSFProfile{}, ErrInvalidCSF
	}
	profile.RFC = matches[1]

	// Extract Código Postal
	matches = codigoPostalRegex.FindStringSubmatch(text)
	if len(matches) < 2 {
		return CSFProfile{}, ErrInvalidCSF
	}
	profile.CodigoPostal = matches[1]

	// Extract Razón Social
	matches = razonSocialRegex.FindStringSubmatch(text)
	if len(matches) < 2 {
		profile.RazonSocial = "SIN RAZON SOCIAL"
	} else {
		profile.RazonSocial = matches[1]
	}

	// Extract Estatus en el padrón
	matches = estatusRegex.FindStringSubmatch(text)
	if len(matches) < 2 {
		profile.EstatusPadron = "ACTIVO"
	} else {
		profile.EstatusPadron = matches[1]
	}

	// Extract Regímenes
	regimenMatches := regimenRegex.FindAllStringSubmatch(text, -1)
	for _, match := range regimenMatches {
		if len(match) < 3 {
			continue
		}
		profile.Regimenes = append(profile.Regimenes, RegimenFiscal{
			Clave:       match[1],
			Descripcion: match[2],
		})
	}

	// Determine IsResico
	for _, regimen := range profile.Regimenes {
		if regimen.Clave == "626" {
			profile.IsResico = true
			break
		}
	}

	return profile, nil
}
