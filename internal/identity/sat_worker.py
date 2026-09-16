import csv
import hashlib
import io
from dataclasses import dataclass
from typing import List, Tuple, Optional
import unicodedata

from internal.identity.blacklist import BlacklistService, BlacklistStatus

@dataclass(frozen=True)
class SyncResult:
    status: str
    total_records_parsed: int
    records_updated: int
    sha256_hash: str

@dataclass(frozen=True)
class FlaggedMerchant:
    rfc: str
    status: BlacklistStatus
    oficio_sat: str

class SATIngestionWorker:
    def __init__(
        self,
        blacklist_service: BlacklistService,
        default_url: str = "http://omawww.sat.gob.mx/cifras_sat/Documents/Listado_Completo_69-B.csv"
    ):
        self.blacklist_service = blacklist_service
        self.default_url = default_url
        self.last_hash: Optional[str] = None

    def _normalize_text(self, text: str) -> str:
        """Strip accents and convert to upper case for fuzzy matching."""
        nfkd = unicodedata.normalize('NFKD', text)
        return "".join([c for c in nfkd if not unicodedata.combining(c)]).strip().upper()

    def _map_situation(self, situation_str: str) -> Optional[BlacklistStatus]:
        norm = self._normalize_text(situation_str)
        if "DEFINITIVO" in norm:
            return BlacklistStatus.DEFINITIVO
        if "PRESUNTO" in norm:
            return BlacklistStatus.PRESUNTO
        if "DESVIRTUADO" in norm:
            return BlacklistStatus.DESVIRTUADO
        if "FAVORABLE" in norm or "SENTENCIA" in norm:
            return BlacklistStatus.SENTENCIA_FAVORABLE
        return None

    def parse_csv_content(self, csv_bytes: bytes) -> List[Tuple[str, BlacklistStatus, str]]:
        # 1. Decode bytes (try utf-8, fallback to latin-1 / iso-8859-1)
        try:
            content = csv_bytes.decode("utf-8-sig")
        except UnicodeDecodeError:
            content = csv_bytes.decode("latin-1")

        lines = content.splitlines()
        
        # 2. Find header row containing 'RFC'
        header_idx = -1
        delimiter = ","
        for idx, line in enumerate(lines):
            if "RFC" in line.upper():
                header_idx = idx
                delimiter = ";" if ";" in line else ","
                break

        if header_idx == -1:
            raise ValueError("No valid header with 'RFC' found in CSV content.")

        # 3. Parse CSV rows starting from header row
        csv_file = io.StringIO("\n".join(lines[header_idx:]))
        reader = csv.reader(csv_file, delimiter=delimiter)
        
        header = [self._normalize_text(col) for col in next(reader)]
        
        # Locate relevant column indices
        rfc_idx = next(i for i, col in enumerate(header) if "RFC" in col)
        
        # Situation index
        sit_idx = next((i for i, col in enumerate(header) if "SITUACI" in col), 3)

        records: List[Tuple[str, BlacklistStatus, str]] = []

        for row in reader:
            if not row or len(row) <= rfc_idx:
                continue

            rfc = row[rfc_idx].strip().upper()
            if not rfc or len(rfc) < 12:
                continue

            situation_raw = row[sit_idx].strip() if len(row) > sit_idx else ""
            status = self._map_situation(situation_raw)
            if not status:
                continue

            # Find the most relevant oficio from the row
            oficio = ""
            for cell in row[4:]:
                cell_clean = cell.strip()
                if cell_clean and ("500-" in cell_clean or "OFICIO" in cell_clean.upper() or "/" in cell_clean):
                    oficio = cell_clean
                    break

            records.append((rfc, status, oficio))

        return records

    def sync_from_bytes(self, csv_bytes: bytes, force: bool = False) -> SyncResult:
        current_hash = hashlib.sha256(csv_bytes).hexdigest()

        if not force and self.last_hash == current_hash:
            return SyncResult(
                status="SKIPPED_UNCHANGED",
                total_records_parsed=0,
                records_updated=0,
                sha256_hash=current_hash
            )

        records = self.parse_csv_content(csv_bytes)
        for rfc, status, oficio in records:
            self.blacklist_service.add_entry(rfc, status, oficio_sat=oficio)

        self.last_hash = current_hash
        return SyncResult(
            status="SUCCESS",
            total_records_parsed=len(records),
            records_updated=len(records),
            sha256_hash=current_hash
        )

    def retroscreen_merchants(self, active_rfcs: List[str]) -> List[FlaggedMerchant]:
        flagged: List[FlaggedMerchant] = []
        for rfc in active_rfcs:
            status = self.blacklist_service.get_status(rfc)
            if status in (BlacklistStatus.DEFINITIVO, BlacklistStatus.PRESUNTO):
                oficio = self.blacklist_service.get_oficio(rfc)
                flagged.append(FlaggedMerchant(rfc=rfc, status=status, oficio_sat=oficio))
        return flagged
