package mx.softpos.app.ui.vault

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class TaxVaultPresenterTest {

    @Test
    fun `buildVaultState computes RESICO rate tier and total tax escrow cents`() {
        val state = TaxVaultPresenter.buildVaultState(
            merchantRfc = "GODE561231GR8",
            periodYearMonth = "2026-10",
            monthlySalesCents = 4_000_000L,
            ivaEscrowCents = 551_724L,
            isrEscrowCents = 37_931L,
            cfdiCount = 42
        )

        assertEquals("GODE561231GR8", state.merchantRfc)
        assertEquals("2026-10", state.periodYearMonth)
        assertEquals(4_000_000L, state.monthlySalesCents)
        assertEquals(110L, state.currentResicoRateBps)
        assertEquals(551_724L, state.ivaEscrowCents)
        assertEquals(37_931L, state.isrEscrowCents)
        assertEquals(589_655L, state.totalTaxEscrowCents)
        assertEquals(42, state.cfdiCount)
    }

    @Test
    fun `buildAccountantPackage formats CSV header and values accurately`() {
        val state = TaxVaultPresenter.buildVaultState(
            merchantRfc = "GODE561231GR8",
            periodYearMonth = "2026-10",
            monthlySalesCents = 2_000_000L,
            ivaEscrowCents = 275_862L,
            isrEscrowCents = 17_241L,
            cfdiCount = 2
        )
        val uuids = listOf("uuid-cfdi-001", "uuid-cfdi-002")

        val pkg = TaxVaultPresenter.buildAccountantPackage(state, uuids)

        assertEquals("GODE561231GR8", pkg.merchantRfc)
        assertEquals("2026-10", pkg.periodYearMonth)
        assertEquals(293_103L, pkg.totalTaxEscrowCents)
        assertEquals(275_862L, pkg.ivaEscrowCents)
        assertEquals(17_241L, pkg.isrEscrowCents)
        assertEquals(100L, pkg.resicoTierBps)
        assertEquals(uuids, pkg.cfdiUuids)

        val expectedCsv = "RFC,PERIODO,VENTAS_CENTS,IVA_ESCROW_CENTS,ISR_ESCROW_CENTS,TOTAL_ESCROW_CENTS,TASA_BPS,CFDI_COUNT\n" +
            "GODE561231GR8,2026-10,2000000,275862,17241,293103,100,2"
        assertEquals(expectedCsv, pkg.summaryCsv)
    }

    @Test
    fun `buildVaultState rejects negative inputs`() {
        assertThrows(IllegalArgumentException::class.java) {
            TaxVaultPresenter.buildVaultState("GODE561231GR8", "2026-10", -1L, 0L, 0L, 0)
        }
        assertThrows(IllegalArgumentException::class.java) {
            TaxVaultPresenter.buildVaultState("GODE561231GR8", "2026-10", 100L, -1L, 0L, 0)
        }
        assertThrows(IllegalArgumentException::class.java) {
            TaxVaultPresenter.buildVaultState("GODE561231GR8", "2026-10", 100L, 0L, -1L, 0)
        }
        assertThrows(IllegalArgumentException::class.java) {
            TaxVaultPresenter.buildVaultState("GODE561231GR8", "2026-10", 100L, 0L, 0L, -1)
        }
    }
}
