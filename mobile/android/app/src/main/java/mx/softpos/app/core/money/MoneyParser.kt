package mx.softpos.app.core.money

/** Parses what a merchant types ("50", "50.5", "1,250.75") into Long cents, never via floats. */
object MoneyParser {
    private val pattern = Regex("^(\\d{1,3}(,\\d{3})*|\\d+)([.](\\d{1,2}))?$")

    /** Null for blank, malformed, more than 2 decimals or above [maxCents]. */
    fun parseMxnToCents(input: String, maxCents: Long = 100_000_000_000L): Long? {
        val text = input.trim().removePrefix("$").trim()
        val m = pattern.matchEntire(text) ?: return null
        val pesos = m.groupValues[1].replace(",", "")
        if (pesos.length > 12) return null
        val decimals = m.groupValues[4].padEnd(2, '0')
        val cents = pesos.toLong() * 100L + (if (decimals.isEmpty()) 0L else decimals.toLong())
        return cents.takeIf { it <= maxCents }
    }
}
