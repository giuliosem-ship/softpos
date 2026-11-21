package mx.softpos.app.contract

/**
 * Response bodies stored under src/test/resources/contract. Batch G (kyb_*,
 * card_*, intent_*, sales_*) were captured from the real Go handlers; batch J
 * (csd_*, invoice*, sat_catalogs_*) are handwritten from the agreed contract
 * and must be re-verified against the handlers. Regenerate them when a
 * response struct changes; a decoding failure here is contract drift.
 */
object Fixtures {
    fun read(name: String): String =
        requireNotNull(Fixtures::class.java.getResource("/contract/$name")) { "missing fixture $name" }.readText()
}
