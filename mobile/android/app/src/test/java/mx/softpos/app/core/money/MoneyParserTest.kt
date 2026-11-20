package mx.softpos.app.core.money

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class MoneyParserTest {
    @Test
    fun parsesPesosAndCentavosWithoutFloats() {
        assertEquals(5_000L, MoneyParser.parseMxnToCents("50"))
        assertEquals(5_050L, MoneyParser.parseMxnToCents("50.5"))
        assertEquals(5_001L, MoneyParser.parseMxnToCents("50.01"))
        assertEquals(125_075L, MoneyParser.parseMxnToCents("$1,250.75"))
        assertEquals(10L, MoneyParser.parseMxnToCents("0.10"))
    }

    @Test
    fun rejectsMalformedInput() {
        listOf("", " ", "abc", "1.234", "1..2", "-5", "1,23", "12,34.5").forEach {
            assertNull(MoneyParser.parseMxnToCents(it), it)
        }
        assertNull(MoneyParser.parseMxnToCents("1000000000.01"))
        assertNull(MoneyParser.parseMxnToCents("99999999999999999999"))
    }
}
