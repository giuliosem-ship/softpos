package mx.softpos.app.fiscal

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.math.BigInteger
import java.time.Instant

/** Synthetic certificate from [TestCertificates]: no SAT data, valid 2025-01-01 to 2029-01-01 UTC. */
class CsdInspectorTest {
    private val cer = TestCertificates.csd()

    @Test
    fun readsSerialRfcAndValidity() {
        val s = requireNotNull(CsdInspector.inspect(cer))
        assertEquals("30001000000500003416", s.serial)
        assertEquals("TEST010203AB1", s.rfc)
        assertEquals(Instant.parse("2025-01-01T00:00:00Z"), s.validFrom)
        assertEquals(Instant.parse("2029-01-01T00:00:00Z"), s.validTo)
    }

    @Test
    fun garbageIsUnreadable() {
        assertNull(CsdInspector.inspect(byteArrayOf(1, 2, 3)))
        assertNull(CsdInspector.inspect(ByteArray(0)))
    }

    @Test
    fun nonAsciiSerialFallsBackToHex() {
        assertEquals("1F", CsdInspector.satSerial(BigInteger.valueOf(31)))
    }

    @Test
    fun issuesCoverValidityRfcAndKeyShape() {
        val s = CsdInspector.inspect(cer)
        val inside = Instant.parse("2026-06-01T00:00:00Z")
        val derKey = ByteArray(1200).also { it[0] = 0x30 }
        assertTrue(CsdInspector.issues(s, true, derKey, "test010203ab1", inside).isEmpty())
        assertEquals(listOf(CsdIssue.RFC_MISMATCH), CsdInspector.issues(s, true, derKey, "XAXX010101000", inside))
        assertEquals(listOf(CsdIssue.CER_EXPIRED), CsdInspector.issues(s, true, derKey, null, Instant.parse("2029-01-02T00:00:00Z")))
        assertEquals(listOf(CsdIssue.CER_NOT_YET_VALID), CsdInspector.issues(s, true, derKey, null, Instant.parse("2024-12-31T00:00:00Z")))
        assertEquals(listOf(CsdIssue.KEY_INVALID), CsdInspector.issues(s, true, "-----BEGIN".toByteArray(), null, inside))
        assertEquals(listOf(CsdIssue.CER_UNREADABLE), CsdInspector.issues(null, true, null, null, inside))
        assertFalse(CsdInspector.looksLikeDerKey(ByteArray(CsdInspector.MAX_FILE_BYTES + 1).also { it[0] = 0x30 }))
    }

    @Test
    fun utf8CopyOfPassword() {
        assertEquals("contraseña", String(utf8("contraseña".toCharArray()), Charsets.UTF_8))
    }
}
