import unittest
from internal.identity.csf_parser import (
    parse_csf_text,
    CSFProfile,
    InvalidCSFError
)

SAMPLE_CSF_RESICO = """
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
"""

SAMPLE_CSF_ACTIVIDAD_EMPRESARIAL = """
CEDULA DE IDENTIFICACION FISCAL
RFC: ABC850101XYZ
Denominación/Razón Social: COMERCIALIZADORA DEL VALLE SA DE CV
Código Postal: 64000
Estatus en el padrón: ACTIVO
Regímenes:
Régimen: 601 - General de Ley Personas Morales Fecha inicio: 15/03/2018
"""

class TestCsfParser(unittest.TestCase):
    def test_parse_resico_csf(self):
        profile: CSFProfile = parse_csf_text(SAMPLE_CSF_RESICO)
        self.assertEqual(profile.rfc, "GARM850101ABC")
        self.assertEqual(profile.razon_social, "JUAN PEREZ GARCIA")
        self.assertEqual(profile.codigo_postal, "06700")
        self.assertEqual(profile.estatus_padron, "ACTIVO")
        self.assertTrue(profile.is_resico)
        self.assertEqual(len(profile.regimenes), 1)
        self.assertEqual(profile.regimenes[0]["clave"], "626")

    def test_parse_general_persona_moral(self):
        profile: CSFProfile = parse_csf_text(SAMPLE_CSF_ACTIVIDAD_EMPRESARIAL)
        self.assertEqual(profile.rfc, "ABC850101XYZ")
        self.assertEqual(profile.razon_social, "COMERCIALIZADORA DEL VALLE SA DE CV")
        self.assertEqual(profile.codigo_postal, "64000")
        self.assertFalse(profile.is_resico)
        self.assertEqual(profile.regimenes[0]["clave"], "601")

    def test_missing_rfc_raises(self):
        invalid_text = "Código Postal: 06700 Denominación: TIENDA SA DE CV"
        with self.assertRaises(InvalidCSFError):
            parse_csf_text(invalid_text)

    def test_missing_postal_code_raises(self):
        invalid_text = "RFC: GARM850101ABC Denominación: TIENDA SA DE CV"
        with self.assertRaises(InvalidCSFError):
            parse_csf_text(invalid_text)

if __name__ == "__main__":
    unittest.main()
