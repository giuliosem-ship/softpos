package mx.softpos.app.nfc

import java.util.LinkedHashMap

object EmvTlvCodec {

    private fun Char.isValidHex(): Boolean =
        this in '0'..'9' || this in 'a'..'f' || this in 'A'..'F'

    fun encode(tags: Map<String, String>): String {
        val sb = StringBuilder()
        for ((rawTag, rawValue) in tags) {
            val tag = rawTag.trim().uppercase()
            val value = rawValue.trim().uppercase()
            require(tag.isNotEmpty() && tag.length % 2 == 0 && tag.all { it.isValidHex() }) {
                "Invalid EMV tag hex: $rawTag"
            }
            require(value.length % 2 == 0 && value.all { it.isValidHex() }) {
                "Invalid EMV value hex for tag $tag"
            }
            val byteLength = value.length / 2
            val lengthHex = when {
                byteLength <= 0x7F -> "%02X".format(byteLength)
                byteLength <= 0xFF -> "81%02X".format(byteLength)
                byteLength <= 0xFFFF -> "82%04X".format(byteLength)
                else -> throw IllegalArgumentException("EMV TLV value too large: $byteLength bytes")
            }
            sb.append(tag).append(lengthHex).append(value)
        }
        return sb.toString()
    }

    fun decode(hexPayload: String): Map<String, String> {
        val clean = hexPayload.trim().uppercase()
        require(clean.isNotEmpty() && clean.length % 2 == 0 && clean.all { it.isValidHex() }) {
            "Malformed hex payload"
        }

        val bytes = ByteArray(clean.length / 2) { i ->
            clean.substring(i * 2, i * 2 + 2).toInt(16).toByte()
        }

        val result = LinkedHashMap<String, String>()
        var offset = 0

        while (offset < bytes.size) {
            val tagStart = offset
            val firstTagByte = bytes[offset].toInt() and 0xFF
            offset++

            if ((firstTagByte and 0x1F) == 0x1F) {
                // Multi-byte BER-TLV tag: subsequent bytes have bit 8 = 1 except the last byte
                if (offset >= bytes.size) {
                    throw IllegalArgumentException("Truncated multi-byte EMV tag at offset $tagStart")
                }
                while (offset < bytes.size && (bytes[offset].toInt() and 0x80) != 0) {
                    offset++
                }
                if (offset >= bytes.size) {
                    throw IllegalArgumentException("Unterminated multi-byte EMV tag at offset $tagStart")
                }
                offset++ // Include final byte of multi-byte tag
            }

            val tagHex = clean.substring(tagStart * 2, offset * 2)

            if (offset >= bytes.size) {
                throw IllegalArgumentException("Missing length byte for tag $tagHex")
            }

            val firstLenByte = bytes[offset].toInt() and 0xFF
            offset++

            val valueByteLen = when {
                (firstLenByte and 0x80) == 0 -> firstLenByte
                firstLenByte == 0x81 -> {
                    if (offset >= bytes.size) {
                        throw IllegalArgumentException("Truncated 1-byte extended length for tag $tagHex")
                    }
                    val len = bytes[offset].toInt() and 0xFF
                    offset++
                    len
                }
                firstLenByte == 0x82 -> {
                    if (offset + 1 >= bytes.size) {
                        throw IllegalArgumentException("Truncated 2-byte extended length for tag $tagHex")
                    }
                    val len = ((bytes[offset].toInt() and 0xFF) shl 8) or (bytes[offset + 1].toInt() and 0xFF)
                    offset += 2
                    len
                }
                else -> throw IllegalArgumentException("Unsupported BER-TLV length specifier: 0x${firstLenByte.toString(16)}")
            }

            if (valueByteLen < 0 || offset + valueByteLen > bytes.size) {
                throw IllegalArgumentException("Truncated value for tag $tagHex: expected $valueByteLen bytes")
            }

            val valueHex = clean.substring(offset * 2, (offset + valueByteLen) * 2)
            result[tagHex] = valueHex
            offset += valueByteLen
        }

        return result
    }

    fun maskPan(pan: String): String {
        val normalized = pan.trim()
        require(normalized.length in 12..19 && normalized.all { it.isDigit() }) {
            "PAN must contain between 12 and 19 digits"
        }
        val bin = normalized.substring(0, 6)
        val last4 = normalized.substring(normalized.length - 4)
        val maskedMiddle = "*".repeat(normalized.length - 10)
        return "$bin$maskedMiddle$last4"
    }

    fun zeroize(sensitiveBuffer: ByteArray) {
        sensitiveBuffer.fill(0)
    }
}
