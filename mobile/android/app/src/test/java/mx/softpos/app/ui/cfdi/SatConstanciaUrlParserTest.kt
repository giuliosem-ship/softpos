package mx.softpos.app.ui.cfdi

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SatConstanciaUrlParserTest {

    @Test
    fun `parseQrUrl extracts idCif and 13-char Persona Fisica RFC from official SAT URL`() {
        val url = "https://siat.sat.gob.mx/app/qr/faces/pages/mobile/validadorqr.jsf?D1=10&D2=1&D3=14070123456_gode561231gr8"
        val parsed = SatConstanciaUrlParser.parseQrUrl(url)

        assertEquals("14070123456", parsed.idCif)
        assertEquals("GODE561231GR8", parsed.rfc)
        assertFalse(parsed.isPersonaMoral)
        assertFalse(parsed.isGeneric)
    }

    @Test
    fun `parseQrUrl extracts idCif and 12-char Persona Moral RFC from official SAT URL`() {
        val url = "https://siat.sat.gob.mx/app/qr/faces/pages/mobile/validadorqr.jsf?D1=10&D2=1&D3=22090123456_EKU9003173C9"
        val parsed = SatConstanciaUrlParser.parseQrUrl(url)

        assertEquals("22090123456", parsed.idCif)
        assertEquals("EKU9003173C9", parsed.rfc)
        assertTrue(parsed.isPersonaMoral)
        assertFalse(parsed.isGeneric)
    }

    @Test
    fun `parseQrUrl rejects malformed URLs or invalid RFCs`() {
        assertThrows(IllegalArgumentException::class.java) {
            SatConstanciaUrlParser.parseQrUrl("https://example.com/not-a-sat-qr")
        }
        assertThrows(IllegalArgumentException::class.java) {
            SatConstanciaUrlParser.parseQrUrl("https://siat.sat.gob.mx/app/qr/validadorqr.jsf?D3=ABC_GODE561231GR8")
        }
        assertThrows(IllegalArgumentException::class.java) {
            SatConstanciaUrlParser.parseQrUrl("https://siat.sat.gob.mx/app/qr/validadorqr.jsf?D3=14070123456_INVALID")
        }
    }

    @Test
    fun `publicoEnGeneral returns generic XAXX010101000 profile`() {
        val generic = SatConstanciaUrlParser.publicoEnGeneral()

        assertEquals("00000000000", generic.idCif)
        assertEquals("XAXX010101000", generic.rfc)
        assertFalse(generic.isPersonaMoral)
        assertTrue(generic.isGeneric)
    }
}
