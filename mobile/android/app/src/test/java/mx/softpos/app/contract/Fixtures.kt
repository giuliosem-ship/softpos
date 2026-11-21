package mx.softpos.app.contract

/**
 * Response bodies stored under src/test/resources/contract, all captured from
 * the real Go handlers: batch G (kyb_*, card_*, intent_*, sales_*) and batch J
 * (csd_*, invoice*, sat_catalogs_*). Regenerate them when a response struct
 * changes; a decoding failure here is contract drift.
 */
object Fixtures {
    fun read(name: String): String =
        requireNotNull(Fixtures::class.java.getResource("/contract/$name")) { "missing fixture $name" }.readText()
}
