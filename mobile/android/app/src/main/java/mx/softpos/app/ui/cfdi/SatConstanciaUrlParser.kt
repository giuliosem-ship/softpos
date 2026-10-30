package mx.softpos.app.ui.cfdi

import java.util.regex.Pattern

data class SatCifData(
    val idCif: String,
    val rfc: String,
    val isPersonaMoral: Boolean,
    val isGeneric: Boolean
)

object SatConstanciaUrlParser {

    private const val D3_QUERY_PARAM = "D3="
    private val PERSONA_MORAL_RFC_PATTERN = Pattern.compile("^[A-Z&Ñ]{3}\\d{6}[A-Z0-9]{3}$")
    private val PERSONA_FISICA_RFC_PATTERN = Pattern.compile("^[A-Z&Ñ]{4}\\d{6}[A-Z0-9]{3}$")

    fun parseQrUrl(qrContent: String): SatCifData {
        val d3Index = qrContent.indexOf(D3_QUERY_PARAM)
        if (d3Index == -1) {
            throw IllegalArgumentException("Invalid QR URL: D3 parameter not found")
        }

        val d3Value = qrContent.substring(d3Index + D3_QUERY_PARAM.length)
        val ampersandIndex = d3Value.indexOf('&')
        val hashIndex = d3Value.indexOf('#')

        val d3Substring = when {
            ampersandIndex != -1 -> d3Value.substring(0, ampersandIndex)
            hashIndex != -1 -> d3Value.substring(0, hashIndex)
            else -> d3Value
        }

        val parts = d3Substring.split('_')
        if (parts.size != 2) {
            throw IllegalArgumentException("Invalid D3 value format")
        }

        val idCif = parts[0]
        if (idCif.isEmpty() || !idCif.all { it.isDigit() }) {
            throw IllegalArgumentException("Invalid idCif: $idCif")
        }

        val rawRfc = parts[1]
        val rfc = rawRfc.trim().uppercase()

        val isPersonaMoral = PERSONA_MORAL_RFC_PATTERN.matcher(rfc).matches()
        val isPersonaFisica = PERSONA_FISICA_RFC_PATTERN.matcher(rfc).matches()

        if (!isPersonaMoral && !isPersonaFisica) {
            throw IllegalArgumentException("Invalid RFC: $rfc")
        }

        val isGeneric = rfc == "XAXX010101000" || rfc == "XEXX010101000"

        return SatCifData(idCif = idCif, rfc = rfc, isPersonaMoral = isPersonaMoral, isGeneric = isGeneric)
    }

    fun publicoEnGeneral(): SatCifData {
        return SatCifData(idCif = "00000000000", rfc = "XAXX010101000", isPersonaMoral = false, isGeneric = true)
    }
}
