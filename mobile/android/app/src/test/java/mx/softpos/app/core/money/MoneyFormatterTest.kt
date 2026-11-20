package mx.softpos.app.core.money

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class MoneyFormatterTest {
    @Test
    fun formatsCentsWithGrouping() {
        assertEquals("$0.00", MoneyFormatter.formatMxn(0))
        assertEquals("$0.05", MoneyFormatter.formatMxn(5))
        assertEquals("$12.34", MoneyFormatter.formatMxn(1_234))
        assertEquals("$1,234.56", MoneyFormatter.formatMxn(123_456))
        assertEquals("$9,999,999.99", MoneyFormatter.formatMxn(999_999_999))
        assertEquals("-$1,000.00", MoneyFormatter.formatMxn(-100_000))
        assertEquals("$92,233,720,368,547,758.07", MoneyFormatter.formatMxn(Long.MAX_VALUE))
    }
}
