from enum import Enum
from dataclasses import dataclass, field
from typing import Dict

class BlacklistStatus(Enum):
    CLEAN = "CLEAN"
    PRESUNTO = "PRESUNTO"
    DESVIRTUADO = "DESVIRTUADO"
    DEFINITIVO = "DEFINITIVO"
    SENTENCIA_FAVORABLE = "SENTENCIA_FAVORABLE"

class BlacklistedEntityError(Exception):
    pass

@dataclass(frozen=True)
class BlacklistEntry:
    rfc: str
    status: BlacklistStatus
    oficio_sat: str = ""

@dataclass(frozen=True)
class BlacklistService:
    _blacklist: Dict[str, BlacklistEntry] = field(default_factory=dict)

    def add_entry(self, rfc: str, status: BlacklistStatus, oficio_sat: str = '') -> None:
        entry = BlacklistEntry(rfc=rfc, status=status, oficio_sat=oficio_sat)
        self._blacklist[rfc] = entry

    def get_status(self, rfc: str) -> BlacklistStatus:
        return self._blacklist.get(rfc, BlacklistEntry(rfc=rfc, status=BlacklistStatus.CLEAN)).status

    def get_entry(self, rfc: str):
        return self._blacklist.get(rfc)

    def get_oficio(self, rfc: str) -> str:
        entry = self._blacklist.get(rfc)
        return entry.oficio_sat if entry else ""

    def validate_merchant_eligibility(self, rfc: str) -> bool:
        status = self.get_status(rfc)
        if status in {BlacklistStatus.DEFINITIVO, BlacklistStatus.PRESUNTO}:
            raise BlacklistedEntityError(f"RFC {rfc} is blacklisted with status: {status.name}")
        return True
