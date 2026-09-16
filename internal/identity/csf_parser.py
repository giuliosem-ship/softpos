import re
from dataclasses import dataclass, field
from typing import List, Dict

class InvalidCSFError(Exception):
    """Custom exception for invalid CSF text."""
    pass

@dataclass(frozen=True)
class CSFProfile:
    rfc: str
    razon_social: str
    codigo_postal: str
    regimenes: List[Dict[str, str]]
    estatus_padron: str
    is_resico: bool

def parse_csf_text(text: str) -> CSFProfile:
    # Extract RFC
    rfc_match = re.search(r'RFC:\s*([A-Z0-9]{12,13})', text)
    if not rfc_match:
        raise InvalidCSFError("RFC is missing in the CSF text.")
    rfc = rfc_match.group(1).strip()

    # Extract Razon Social
    razon_social_match = re.search(r'Denominación/Razón Social:\s*(.+?)(?:\n|Régimen|Datos)', text)
    razon_social = razon_social_match.group(1).strip() if razon_social_match else 'SIN RAZON SOCIAL'

    # Extract Código Postal
    codigo_postal_match = re.search(r'Código Postal:\s*(\d{5})', text)
    if not codigo_postal_match:
        raise InvalidCSFError("Código Postal is missing in the CSF text.")
    codigo_postal = codigo_postal_match.group(1).strip()

    # Extract Estatus en el padrón
    estatus_padron_match = re.search(r'Estatus en el padrón:\s*([A-Z]+)', text)
    estatus_padron = estatus_padron_match.group(1).strip() if estatus_padron_match else 'ACTIVO'

    # Extract Regímenes
    regimen_matches = re.findall(r'Régimen:\s*(\d{3})\s*-\s*([^\n\r]+?)(?:\s+Fecha|\n|$)', text)
    regimenes = [{"clave": clave.strip(), "descripcion": descripcion.strip()} for clave, descripcion in regimen_matches]

    # Determine is_resico
    is_resico = any(regimen["clave"] == "626" for regimen in regimenes)

    return CSFProfile(
        rfc=rfc,
        razon_social=razon_social,
        codigo_postal=codigo_postal,
        regimenes=regimenes,
        estatus_padron=estatus_padron,
        is_resico=is_resico
    )
