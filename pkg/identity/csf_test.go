package identity

import (
	"testing"
)

const sampleCSFResico = `
CEDULA DE IDENTIFICACION FISCAL
RFC: GARM850101ABC
Denominación/Razón Social: JUAN PEREZ GARCIA
Régimen Capital: 
Datos del Domicilio Registrado
Código Postal: 06700
Tipo de Vialidad: AVENIDA
Nombre de Vialidad: INSURGENTES SUR
Estatus en el padrón: ACTIVO
Fecha de inicio de operaciones: 01 de Enero de 2022
Regímenes:
Régimen: 626 - Régimen Simplificado de Confianza Fecha inicio: 01/01/2022
`

const sampleCSFActividadEmpresarial = `
CEDULA DE IDENTIFICACION FISCAL
RFC: ABC850101XYZ
Denominación/Razón Social: COMERCIALIZADORA DEL VALLE SA DE CV
Código Postal: 64000
Estatus en el padrón: ACTIVO
Regímenes:
Régimen: 601 - General de Ley Personas Morales Fecha inicio: 15/03/2018
`

func TestParseResicoCSF(t *testing.T) {
	profile, err := ParseCSFText(sampleCSFResico)
	if err != nil {
		t.Fatalf("unexpected error parsing CSF: %v", err)
	}
	if profile.RFC != "GARM850101ABC" {
		t.Errorf("expected RFC GARM850101ABC, got %s", profile.RFC)
	}
	if profile.RazonSocial != "JUAN PEREZ GARCIA" {
		t.Errorf("expected RazonSocial JUAN PEREZ GARCIA, got %s", profile.RazonSocial)
	}
	if profile.CodigoPostal != "06700" {
		t.Errorf("expected CodigoPostal 06700, got %s", profile.CodigoPostal)
	}
	if profile.EstatusPadron != "ACTIVO" {
		t.Errorf("expected EstatusPadron ACTIVO, got %s", profile.EstatusPadron)
	}
	if !profile.IsResico {
		t.Errorf("expected IsResico true")
	}
	if len(profile.Regimenes) != 1 || profile.Regimenes[0].Clave != "626" {
		t.Errorf("expected regimen 626, got %+v", profile.Regimenes)
	}
}

func TestParseGeneralPersonaMoral(t *testing.T) {
	profile, err := ParseCSFText(sampleCSFActividadEmpresarial)
	if err != nil {
		t.Fatalf("unexpected error parsing CSF: %v", err)
	}
	if profile.RFC != "ABC850101XYZ" {
		t.Errorf("expected RFC ABC850101XYZ, got %s", profile.RFC)
	}
	if profile.RazonSocial != "COMERCIALIZADORA DEL VALLE SA DE CV" {
		t.Errorf("expected RazonSocial COMERCIALIZADORA DEL VALLE SA DE CV, got %s", profile.RazonSocial)
	}
	if profile.CodigoPostal != "64000" {
		t.Errorf("expected CodigoPostal 64000, got %s", profile.CodigoPostal)
	}
	if profile.IsResico {
		t.Errorf("expected IsResico false")
	}
	if len(profile.Regimenes) != 1 || profile.Regimenes[0].Clave != "601" {
		t.Errorf("expected regimen 601, got %+v", profile.Regimenes)
	}
}

func TestMissingRFCRaises(t *testing.T) {
	invalidText := "Código Postal: 06700 Denominación: TIENDA SA DE CV"
	_, err := ParseCSFText(invalidText)
	if err != ErrInvalidCSF {
		t.Errorf("expected ErrInvalidCSF, got %v", err)
	}
}

func TestMissingPostalCodeRaises(t *testing.T) {
	invalidText := "RFC: GARM850101ABC Denominación: TIENDA SA DE CV"
	_, err := ParseCSFText(invalidText)
	if err != ErrInvalidCSF {
		t.Errorf("expected ErrInvalidCSF, got %v", err)
	}
}
