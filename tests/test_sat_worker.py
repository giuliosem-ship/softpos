import unittest
from internal.identity.blacklist import BlacklistService, BlacklistStatus
from internal.identity.sat_worker import (
    SATIngestionWorker,
    SyncResult,
    FlaggedMerchant
)

# Simulated SAT 69-B official CSV snippet (with BOM / Latin-1 accents)
SAMPLE_SAT_CSV_LATIN1 = """Listado completo de contribuyentes que se ubican en el supuesto del articulo 69-B del Codigo Fiscal de la Federacion
Datos actualizados al 15 de septiembre de 2026
No,RFC,Nombre del Contribuyente,Situación del contribuyente,Número y fecha de oficio global de presunción,Publicación DOF presuntos,Número y fecha de oficio global de definitivos,Publicación DOF definitivos,Número y fecha de oficio global de sentencia favorable,Publicación DOF sentencia favorable,Número y fecha de oficio global de desvirtuados,Publicación DOF desvirtuados
1,EFO110101XYZ,EMPRESA FACTURADORA SA DE CV,Definitivo,500-05-2023-111,01/02/2023,500-05-2023-999,15/06/2023,,,,
2,PRE220202ABC,SERVICIOS CORPORATIVOS DEL NORTE SC,Presunto,500-05-2024-222,10/01/2024,,,,,,
3,DES330303QWE,DISTRIBUIDORA AGRICOLA SA,Desvirtuado,500-05-2023-333,05/03/2023,,,,,500-05-2024-777,20/04/2024
4,SEN440404RTY,LOGISTICA Y TRANSPORTES SC,Sentencia Favorable,500-05-2022-444,12/04/2022,,,,500-05-2023-888,18/11/2023,,
""".encode("iso-8859-1")

class TestSATWorker(unittest.TestCase):
    def setUp(self):
        self.blacklist_service = BlacklistService()
        self.worker = SATIngestionWorker(blacklist_service=self.blacklist_service)

    def test_parse_csv_content(self):
        records = self.worker.parse_csv_content(SAMPLE_SAT_CSV_LATIN1)
        self.assertEqual(len(records), 4)

        # Check records mapping
        rfcs_map = {r[0]: (r[1], r[2]) for r in records}
        self.assertIn("EFO110101XYZ", rfcs_map)
        self.assertEqual(rfcs_map["EFO110101XYZ"][0], BlacklistStatus.DEFINITIVO)
        self.assertEqual(rfcs_map["PRE220202ABC"][0], BlacklistStatus.PRESUNTO)
        self.assertEqual(rfcs_map["DES330303QWE"][0], BlacklistStatus.DESVIRTUADO)
        self.assertEqual(rfcs_map["SEN440404RTY"][0], BlacklistStatus.SENTENCIA_FAVORABLE)

    def test_sync_from_bytes_and_hash_caching(self):
        # 1. First sync -> SUCCESS
        res1: SyncResult = self.worker.sync_from_bytes(SAMPLE_SAT_CSV_LATIN1)
        self.assertEqual(res1.status, "SUCCESS")
        self.assertEqual(res1.total_records_parsed, 4)
        self.assertEqual(self.blacklist_service.get_status("EFO110101XYZ"), BlacklistStatus.DEFINITIVO)

        # 2. Second sync with identical bytes -> SKIPPED_UNCHANGED
        res2: SyncResult = self.worker.sync_from_bytes(SAMPLE_SAT_CSV_LATIN1)
        self.assertEqual(res2.status, "SKIPPED_UNCHANGED")
        self.assertEqual(res2.total_records_parsed, 0)

        # 3. Forced sync with identical bytes -> SUCCESS
        res3: SyncResult = self.worker.sync_from_bytes(SAMPLE_SAT_CSV_LATIN1, force=True)
        self.assertEqual(res3.status, "SUCCESS")
        self.assertEqual(res3.total_records_parsed, 4)

    def test_retroscreen_active_merchants(self):
        # Ingest blacklist first
        self.worker.sync_from_bytes(SAMPLE_SAT_CSV_LATIN1)

        # Active merchants in platform
        active_merchants = [
            "CLEAN_MERCHANT_RFC",  # Not on list -> Clean
            "EFO110101XYZ",       # On list as DEFINITIVO -> Must flag
            "PRE220202ABC",       # On list as PRESUNTO -> Must flag
            "DES330303QWE",       # On list as DESVIRTUADO -> Eligible, do not flag
            "SEN440404RTY"        # On list as SENTENCIA_FAVORABLE -> Eligible, do not flag
        ]

        flagged = self.worker.retroscreen_merchants(active_merchants)
        flagged_rfcs = [f.rfc for f in flagged]

        self.assertEqual(len(flagged), 2)
        self.assertIn("EFO110101XYZ", flagged_rfcs)
        self.assertIn("PRE220202ABC", flagged_rfcs)
        self.assertNotIn("CLEAN_MERCHANT_RFC", flagged_rfcs)
        self.assertNotIn("DES330303QWE", flagged_rfcs)
        self.assertNotIn("SEN440404RTY", flagged_rfcs)

if __name__ == "__main__":
    unittest.main()
