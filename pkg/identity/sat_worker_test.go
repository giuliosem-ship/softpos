package identity

import (
	"testing"
)

// Sample SAT CSV content
var sampleSATCSV = []byte(`Listado completo de contribuyentes que se ubican en el supuesto del articulo 69-B del Codigo Fiscal de la Federacion
Datos actualizados al 15 de septiembre de 2026
No,RFC,Nombre del Contribuyente,Situación del contribuyente,Número y fecha de oficio global de presunción,Publicación DOF presuntos,Número y fecha de oficio global de definitivos,Publicación DOF definitivos,Número y fecha de oficio global de sentencia favorable,Publicación DOF sentencia favorable,Número y fecha de oficio global de desvirtuados,Publicación DOF desvirtuados
1,EFO110101XYZ,EMPRESA FACTURADORA SA DE CV,Definitivo,500-05-2023-111,01/02/2023,500-05-2023-999,15/06/2023,,,,
2,PRE220202ABC,SERVICIOS CORPORATIVOS DEL NORTE SC,Presunto,500-05-2024-222,10/01/2024,,,,,,
3,DES330303QWE,DISTRIBUIDORA AGRICOLA SA,Desvirtuado,500-05-2023-333,05/03/2023,,,,,500-05-2024-777,20/04/2024
4,SEN440404RTY,LOGISTICA Y TRANSPORTES SC,Sentencia Favorable,500-05-2022-444,12/04/2022,,,,500-05-2023-888,18/11/2023,,
`)

func TestParseCSVContent(t *testing.T) {
	service := NewBlacklistService()
	worker := NewSATIngestionWorker(service)

	records, err := worker.ParseCSVContent(sampleSATCSV)
	if err != nil {
		t.Fatalf("unexpected error parsing CSV: %v", err)
	}

	if len(records) != 4 {
		t.Fatalf("expected 4 records, got %d", len(records))
	}

	m := make(map[string]BlacklistStatus)
	for _, r := range records {
		m[r.RFC] = r.Status
	}

	if m["EFO110101XYZ"] != BlacklistDefinitivo {
		t.Errorf("expected DEFINITIVO, got %v", m["EFO110101XYZ"])
	}
	if m["PRE220202ABC"] != BlacklistPresunto {
		t.Errorf("expected PRESUNTO, got %v", m["PRE220202ABC"])
	}
	if m["DES330303QWE"] != BlacklistDesvirtuado {
		t.Errorf("expected DESVIRTUADO, got %v", m["DES330303QWE"])
	}
	if m["SEN440404RTY"] != BlacklistSentenciaFavorable {
		t.Errorf("expected SENTENCIA_FAVORABLE, got %v", m["SEN440404RTY"])
	}
}

func TestSyncFromBytesAndCaching(t *testing.T) {
	service := NewBlacklistService()
	worker := NewSATIngestionWorker(service)

	// 1. First sync -> SUCCESS
	res1, err := worker.SyncFromBytes(sampleSATCSV, false)
	if err != nil {
		t.Fatalf("unexpected error on first sync: %v", err)
	}
	if res1.Status != "SUCCESS" || res1.TotalRecordsParsed != 4 {
		t.Errorf("expected SUCCESS with 4 records, got %+v", res1)
	}
	if service.GetStatus("EFO110101XYZ") != BlacklistDefinitivo {
		t.Errorf("expected DEFINITIVO in blacklist service")
	}

	// 2. Second sync with identical bytes -> SKIPPED_UNCHANGED
	res2, err := worker.SyncFromBytes(sampleSATCSV, false)
	if err != nil {
		t.Fatalf("unexpected error on second sync: %v", err)
	}
	if res2.Status != "SKIPPED_UNCHANGED" || res2.TotalRecordsParsed != 0 {
		t.Errorf("expected SKIPPED_UNCHANGED with 0 records, got %+v", res2)
	}

	// 3. Forced sync -> SUCCESS
	res3, err := worker.SyncFromBytes(sampleSATCSV, true)
	if err != nil {
		t.Fatalf("unexpected error on forced sync: %v", err)
	}
	if res3.Status != "SUCCESS" || res3.TotalRecordsParsed != 4 {
		t.Errorf("expected SUCCESS on forced sync, got %+v", res3)
	}
}

func TestRetroscreenActiveMerchants(t *testing.T) {
	service := NewBlacklistService()
	worker := NewSATIngestionWorker(service)

	_, _ = worker.SyncFromBytes(sampleSATCSV, false)

	activeMerchants := []string{
		"CLEAN_MERCHANT_RFC",
		"EFO110101XYZ",
		"PRE220202ABC",
		"DES330303QWE",
		"SEN440404RTY",
	}

	flagged := worker.RetroscreenMerchants(activeMerchants)
	if len(flagged) != 2 {
		t.Fatalf("expected 2 flagged merchants, got %d", len(flagged))
	}

	flaggedMap := make(map[string]bool)
	for _, f := range flagged {
		flaggedMap[f.RFC] = true
	}

	if !flaggedMap["EFO110101XYZ"] || !flaggedMap["PRE220202ABC"] {
		t.Errorf("expected EFO110101XYZ and PRE220202ABC to be flagged, got %+v", flaggedMap)
	}
}
