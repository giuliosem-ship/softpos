package mx.softpos.app.contract

/**
 * Response bodies captured from the real Go handlers (pkg/api, batch G) and
 * stored under src/test/resources/contract. Regenerate them from the backend
 * tests when a response struct changes; a decoding failure here is contract drift.
 */
object Fixtures {
    fun read(name: String): String =
        requireNotNull(Fixtures::class.java.getResource("/contract/$name")) { "missing fixture $name" }.readText()
}
