package mx.softpos.app.fiscal

import mx.softpos.app.ui.onboarding.KybOnboardingPresenter
import java.io.ByteArrayInputStream
import java.math.BigInteger
import java.security.cert.CertificateException
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.time.Instant
import javax.security.auth.x500.X500Principal

/** What the app can read from a SAT `.cer` (public data) before uploading it. */
data class CertificateSummary(
    /** SAT "número de certificado": the serial's ASCII digits (20 chars) when SAT-encoded, else hex. */
    val serial: String,
    /** RFC of the holder from x500UniqueIdentifier (2.5.4.45, "RFC / CURP"), null if absent. */
    val rfc: String?,
    val validFrom: Instant,
    val validTo: Instant
)

enum class CsdIssue { CER_UNREADABLE, CER_EXPIRED, CER_NOT_YET_VALID, RFC_MISMATCH, KEY_INVALID, PASSWORD_REQUIRED }

/**
 * Client-side pre-checks of a CSD pair. Advisory only: the backend decrypts
 * the key with the password, checks the pair matches and that it is a CSD
 * (not a FIEL), and is the authority (`422 CSD_INVALID`).
 */
object CsdInspector {
    private const val UNIQUE_ID_OID = "2.5.4.45"

    fun inspect(cer: ByteArray): CertificateSummary? = try {
        val cert = CertificateFactory.getInstance("X.509")
            .generateCertificate(ByteArrayInputStream(cer)) as X509Certificate
        CertificateSummary(
            serial = satSerial(cert.serialNumber),
            rfc = holderRfc(cert.subjectX500Principal),
            validFrom = cert.notBefore.toInstant(),
            validTo = cert.notAfter.toInstant()
        )
    } catch (_: CertificateException) {
        null
    } catch (_: ClassCastException) {
        null
    } catch (_: IllegalArgumentException) {
        null
    }

    /** SAT encodes the 20-digit certificate number as the ASCII bytes of the serial. */
    fun satSerial(serial: BigInteger): String {
        val bytes = serial.toByteArray().dropWhile { it == 0.toByte() }
        val ascii = bytes.isNotEmpty() && bytes.all { it in '0'.code.toByte()..'9'.code.toByte() }
        return if (ascii) String(bytes.toByteArray(), Charsets.US_ASCII) else serial.toString(16).uppercase()
    }

    internal fun holderRfc(subject: X500Principal): String? {
        val name = subject.getName(X500Principal.RFC2253)
        val uniqueId = name.split(',').map { it.trim() }
            .firstOrNull { it.startsWith("$UNIQUE_ID_OID=") }
            ?.substringAfter('=')
            ?.let(::decodeAttributeValue)
        // "RFC / CURP" (persona física) or "RFC / RFC representante" (moral): the first token is the holder.
        val candidate = uniqueId?.substringBefore('/')?.trim()?.uppercase()
        // No guessing from other attributes (a CURP contains an RFC-shaped prefix): null skips the check.
        return candidate?.takeIf { KybOnboardingPresenter.isValidRfc(it) }
    }

    /** RFC 2253 prints unknown attribute types as `#` + DER hex (PrintableString / UTF8String). */
    private fun decodeAttributeValue(value: String): String {
        if (!value.startsWith("#")) return value.replace("\\", "")
        val der = value.drop(1).chunked(2).mapNotNull { it.toIntOrNull(16)?.toByte() }.toByteArray()
        if (der.size < 2) return ""
        var offset = 2
        var length = der[1].toInt() and 0xFF
        if (length and 0x80 != 0) {
            val n = length and 0x7F
            length = 0
            repeat(n) { length = (length shl 8) or (der.getOrElse(offset++) { 0 }.toInt() and 0xFF) }
        }
        val end = minOf(der.size, offset + length)
        return if (offset >= end) "" else String(der, offset, end - offset, Charsets.UTF_8)
    }

    /** A SAT `.key` is a DER PKCS#8 EncryptedPrivateKeyInfo: a SEQUENCE (0x30), ~1.2 KB. */
    fun looksLikeDerKey(key: ByteArray): Boolean = key.size in 64..MAX_FILE_BYTES && key[0] == 0x30.toByte()

    fun issues(
        cer: CertificateSummary?,
        cerPicked: Boolean,
        key: ByteArray?,
        merchantRfc: String?,
        now: Instant
    ): List<CsdIssue> = buildList {
        if (cerPicked && cer == null) add(CsdIssue.CER_UNREADABLE)
        if (cer != null) {
            if (now.isAfter(cer.validTo)) add(CsdIssue.CER_EXPIRED)
            if (now.isBefore(cer.validFrom)) add(CsdIssue.CER_NOT_YET_VALID)
            val expected = merchantRfc?.trim()?.uppercase().orEmpty()
            if (expected.isNotEmpty() && cer.rfc != null && cer.rfc != expected) add(CsdIssue.RFC_MISMATCH)
        }
        if (key != null && !looksLikeDerKey(key)) add(CsdIssue.KEY_INVALID)
    }

    /** Larger than any real CSD file (.cer ~1.5 KB, .key ~1.3 KB). */
    const val MAX_FILE_BYTES = 64 * 1024
}
