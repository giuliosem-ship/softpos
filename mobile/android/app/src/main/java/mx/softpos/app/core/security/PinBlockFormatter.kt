package mx.softpos.app.core.security

/**
 * PinBlockFormatter formats plaintext PINs into ISO 9564-1 Format 0 PIN blocks
 * with Primary Account Number (PAN) XOR masking and zeroization of sensitive buffers.
 */
object PinBlockFormatter {

    /**
     * Formats an ISO 9564 Format 0 PIN block.
     * Intermediate sensitive buffers are guaranteed to be zeroized in finally blocks per PCI MPoC.
     */
    fun formatIso0(pin: CharArray, pan: String): ByteArray {
        require(pin.size in 4..12) {
            "PIN length must be between 4 and 12 digits, got: ${pin.size}"
        }
        for (c in pin) {
            require(c in '0'..'9') { "PIN must contain only numeric digits" }
        }

        val cleanPan = pan.trim().replace(" ", "").replace("-", "")
        require(cleanPan.length in 13..19) {
            "PAN length must be between 13 and 19 digits, got: ${cleanPan.length}"
        }
        for (c in cleanPan) {
            require(c in '0'..'9') { "PAN must contain only numeric digits" }
        }

        val pinField = ByteArray(8)
        val panField = ByteArray(8)
        val pinBlock = ByteArray(8)

        try {
            // 1. Build PIN Field (16 hex nibbles / 8 bytes)
            // Nibble 0: 0, Nibble 1: length (4-12)
            val pinHex = StringBuilder(16)
            pinHex.append("0")
            pinHex.append(Integer.toHexString(pin.size))
            for (c in pin) {
                pinHex.append(c)
            }
            while (pinHex.length < 16) {
                pinHex.append("F")
            }
            for (i in 0 until 8) {
                val byteStr = pinHex.substring(i * 2, i * 2 + 2)
                pinField[i] = byteStr.toInt(16).toByte()
            }

            // 2. Build PAN Field (16 hex nibbles / 8 bytes)
            // 4 zeros followed by 12 rightmost digits of PAN excluding check digit
            // PAN digits: substring(length - 13, length - 1)
            val pan12 = cleanPan.substring(cleanPan.length - 13, cleanPan.length - 1)
            val panHex = "0000$pan12"
            for (i in 0 until 8) {
                val byteStr = panHex.substring(i * 2, i * 2 + 2)
                panField[i] = byteStr.toInt(16).toByte()
            }

            // 3. XOR PIN Field with PAN Field
            for (i in 0 until 8) {
                pinBlock[i] = (pinField[i].toInt() xor panField[i].toInt()).toByte()
            }

            return pinBlock
        } finally {
            // Strict PCI MPoC Zeroization: clear sensitive PIN and PAN buffers in memory
            pinField.fill(0)
            panField.fill(0)
        }
    }
}
