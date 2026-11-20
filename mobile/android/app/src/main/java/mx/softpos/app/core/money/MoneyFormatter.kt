package mx.softpos.app.core.money

/** Formats Long cents as pesos without ever going through floating point. */
object MoneyFormatter {
    fun formatMxn(cents: Long): String {
        require(cents != Long.MIN_VALUE) { "cents out of range" }
        val negative = cents < 0
        val abs = if (negative) -cents else cents
        val pesos = (abs / 100L).toString()
        val centavos = (abs % 100L).toString().padStart(2, '0')
        val grouped = StringBuilder()
        pesos.forEachIndexed { index, c ->
            if (index > 0 && (pesos.length - index) % 3 == 0) grouped.append(',')
            grouped.append(c)
        }
        return (if (negative) "-$" else "$") + grouped + "." + centavos
    }
}
