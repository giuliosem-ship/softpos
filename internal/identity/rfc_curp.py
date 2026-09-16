import re
from dataclasses import dataclass, field
from typing import Dict

# Custom exceptions
class InvalidRFCError(Exception):
    """Exception raised for invalid RFC."""
    pass

class InvalidCURPError(Exception):
    """Exception raised for invalid CURP."""
    pass

# RFC validation and parsing
RFC_PF_PATTERN = re.compile(r'^[A-Z]{4}\d{6}[A-Z0-9]{3}$')
RFC_PM_PATTERN = re.compile(r'^[A-Z]{3}\d{6}[A-Z0-9]{3}$')
RFC_GENERIC_PATTERN = re.compile(r'^(XAXX|XEXX)010101000$')

def validate_rfc(rfc: str) -> bool:
    """Validates RFC format."""
    return bool(RFC_GENERIC_PATTERN.match(rfc) or
                RFC_PF_PATTERN.match(rfc) or
                RFC_PM_PATTERN.match(rfc))

def parse_rfc_type(rfc: str) -> str:
    """Returns the type of RFC."""
    if not validate_rfc(rfc):
        raise InvalidRFCError(f"Invalid RFC: {rfc}")
    
    if RFC_GENERIC_PATTERN.match(rfc):
        return "GENERICO"
    elif RFC_PF_PATTERN.match(rfc):
        return "PERSONA_FISICA"
    elif RFC_PM_PATTERN.match(rfc):
        return "PERSONA_MORAL"
    else:
        raise InvalidRFCError(f"Unknown RFC type: {rfc}")

CURP_PATTERN = re.compile(r'^[A-Z]{4}\d{6}[HM][A-Z]{2}[A-Z]{3}[A-Z0-9]\d$')
CURP_GENDER_MAP = {'H': 'HOMBRE', 'M': 'MUJER'}
CURP_STATE_CODES = {
    'AS': 'Aguascalientes', 'BC': 'Baja California', 'BS': 'Baja California Sur',
    'CC': 'Campeche', 'CS': 'Chiapas', 'CH': 'Chihuahua', 'CL': 'Coahuila',
    'CM': 'Colima', 'DF': 'Distrito Federal', 'DG': 'Durango', 'GT': 'Guanajuato',
    'GR': 'Guerrero', 'HG': 'Hidalgo', 'JC': 'Jalisco', 'MC': 'Michoacán',
    'MN': 'Morelos', 'MS': 'Nayarit', 'NL': 'Nuevo León', 'NT': 'Nuevo León', 'NE': 'Nacido en el Extranjero', 'OC': 'Oaxaca',
    'PL': 'Puebla', 'QT': 'Querétaro', 'QR': 'Quintana Roo', 'SP': 'San Luis Potosí',
    'SL': 'Sinaloa', 'SR': 'Sonora', 'TC': 'Tabasco', 'TS': 'Tamaulipas',
    'TL': 'Tlaxcala', 'VZ': 'Veracruz', 'YN': 'Yucatán', 'ZS': 'Zacatecas'
}

def validate_curp(curp: str) -> bool:
    """Validates CURP format."""
    return CURP_PATTERN.match(curp) is not None

def parse_curp(curp: str) -> Dict[str, str]:
    """Parses CURP and returns demographic information."""
    if not validate_curp(curp):
        raise InvalidCURPError(f"Invalid CURP: {curp}")
    
    year = curp[4:6]
    month = curp[6:8]
    day = curp[8:10]
    gender_code = curp[10]
    state_code = curp[11:13]
    
    # Adjust birth year based on the first digit of the year
    if int(year) >= 0 and int(year) <= 20:
        birth_date = f"20{year}-{month.zfill(2)}-{day.zfill(2)}"
    else:
        birth_date = f"19{year}-{month.zfill(2)}-{day.zfill(2)}"
    
    gender = CURP_GENDER_MAP.get(gender_code, "UNKNOWN")
    
    if state_code not in CURP_STATE_CODES:
        raise InvalidCURPError(f"Invalid state code in CURP: {state_code}")
    
    return {
        "birth_date": birth_date,
        "gender": gender,
        "state_code": state_code
    }
