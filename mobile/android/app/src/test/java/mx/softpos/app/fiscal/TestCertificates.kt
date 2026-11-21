package mx.softpos.app.fiscal

import java.io.ByteArrayOutputStream
import java.security.KeyPairGenerator

/**
 * Builds a synthetic X.509 certificate in memory for the CSD tests, so no
 * `.cer` file (git-ignored on purpose) lives in the repo. The signature is
 * junk: the client never verifies it, it only reads serial, subject and
 * validity. Defaults mimic a SAT CSD: serial = ASCII digits, subject
 * x500UniqueIdentifier (2.5.4.45) = "RFC / CURP".
 */
object TestCertificates {
    fun csd(
        uniqueId: String = "TEST010203AB1 / TEST010203HDFRRR09",
        serialAscii: String = "30001000000500003416",
        notBefore: String = "250101000000Z",
        notAfter: String = "290101000000Z"
    ): ByteArray {
        val publicKey = KeyPairGenerator.getInstance("RSA").apply { initialize(1024) }.generateKeyPair().public.encoded
        val sigAlg = seq(oid(1, 2, 840, 113549, 1, 1, 11), byteArrayOf(0x05, 0x00))
        val issuer = name(listOf(intArrayOf(2, 5, 4, 3) to "AC DE PRUEBA"))
        val subject = name(
            listOf(
                intArrayOf(2, 5, 4, 3) to "COMERCIO DE PRUEBA",
                intArrayOf(2, 5, 4, 45) to uniqueId,
                intArrayOf(2, 5, 4, 11) to "Sucursal Prueba"
            )
        )
        val tbs = seq(
            tlv(0xA0, tlv(0x02, byteArrayOf(2))),
            tlv(0x02, serialAscii.toByteArray(Charsets.US_ASCII)),
            sigAlg,
            issuer,
            seq(tlv(0x17, notBefore.toByteArray()), tlv(0x17, notAfter.toByteArray())),
            subject,
            publicKey
        )
        return seq(tbs, sigAlg, tlv(0x03, byteArrayOf(0) + ByteArray(128) { 1 }))
    }

    private fun name(rdns: List<Pair<IntArray, String>>): ByteArray =
        seq(*rdns.map { (o, v) -> tlv(0x31, seq(oid(*o), tlv(0x0C, v.toByteArray(Charsets.UTF_8)))) }.toTypedArray())

    private fun seq(vararg parts: ByteArray): ByteArray = tlv(0x30, parts.fold(ByteArray(0)) { a, b -> a + b })

    private fun oid(vararg arcs: Int): ByteArray {
        val out = ByteArrayOutputStream()
        out.write(arcs[0] * 40 + arcs[1])
        for (arc in arcs.drop(2)) {
            val groups = mutableListOf<Int>()
            var v = arc
            do { groups.add(0, v and 0x7F); v = v ushr 7 } while (v > 0)
            groups.forEachIndexed { i, g -> out.write(if (i < groups.size - 1) g or 0x80 else g) }
        }
        return tlv(0x06, out.toByteArray())
    }

    private fun tlv(tag: Int, content: ByteArray): ByteArray {
        val len = content.size
        val header = when {
            len < 0x80 -> byteArrayOf(tag.toByte(), len.toByte())
            len < 0x100 -> byteArrayOf(tag.toByte(), 0x81.toByte(), len.toByte())
            else -> byteArrayOf(tag.toByte(), 0x82.toByte(), (len shr 8).toByte(), len.toByte())
        }
        return header + content
    }
}
